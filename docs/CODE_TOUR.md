# Code tour

A guided walk through this repository for someone who knows Terraform, PowerShell and Windows DHCP/DNS, but not Go. Read it top to bottom once; afterwards the comments in each file should carry you.

The provider manages two Windows roles, DHCP and DNS, from one provider block. The DHCP side is the reference: section 4 follows a DHCP scope through a `terraform apply` in detail, and section 7 shows what the DNS side does differently.

1. [The big picture](#1-the-big-picture)
2. [Just enough Go to read this repo](#2-just-enough-go-to-read-this-repo)
3. [Map of the repository](#3-map-of-the-repository)
4. [Follow one `terraform apply`](#4-follow-one-terraform-apply)
5. [What actually reaches Windows](#5-what-actually-reaches-windows)
6. [Errors, "not found" and drift](#6-errors-not-found-and-drift)
7. [The DNS side: record sets](#7-the-dns-side-record-sets)
8. [How the tests work](#8-how-the-tests-work)
9. [Common tasks](#9-common-tasks)
10. [Glossary](#10-glossary)

---

## 1. The big picture

A Terraform provider is a separate program. When you run `terraform plan`, Terraform starts `terraform-provider-windowsddi` in the background and talks to it over gRPC (a local network protocol). Terraform says things like "here is the configuration for `windowsddi_dhcp_scope.users`, what would you change?" or "create it now". The provider answers.

This provider answers by running PowerShell on a Windows host: it connects over SSH or WinRM, runs a `DhcpServer` cmdlet such as `Get-DhcpServerv4Scope` or a `DnsServer` cmdlet such as `Get-DnsServerZone`, reads the result back as JSON and turns it into Terraform state.

```
 terraform CLI
      │  gRPC (plugin protocol v6)
      ▼
 main.go ──► internal/provider        provider "windowsddi" {} block, connection setup
                   │ hands a *providerdata.Clients (DHCP client + DNS client) to every resource
                   ▼
   internal/resources/dhcp       windowsddi_dhcp_scope, _reservation, ...      (CRUD + import)
   internal/resources/dns        windowsddi_dns_zone, _a_record_set, ...       (CRUD + import)
   internal/datasources/dhcp|dns data "windowsddi_dhcp_scope", "windowsddi_dns_zones", ... (read only)
                   │                                   │
                   │                                   ▼ record-set resources only
                   │                          internal/recordset   (generic add/diff/remove engine)
                   │ calls typed Go functions:         │
                   │ GetScope, AddScope, GetZone,      │
                   ▼ AddRecord, ...                    ▼
            internal/dhcp   internal/dns      one PowerShell script per operation
                   │                       │
                   ▼                       ▼
            internal/psscript        internal/runner
            builds the script,       sends it over SSH or WinRM,
            module check,            returns what PowerShell printed
            parses the JSON reply          │
                                           ▼
                        Windows host: powershell.exe + DhcpServer / DnsServer module
```

The layering is strict: resources never write PowerShell, and only `dhcp` and `dns` know cmdlet names. `psscript` and `runner` are shared by both services and know nothing about DHCP or DNS.

## 2. Just enough Go to read this repo

You do not need to write Go to follow the code. These are the constructs you will meet, in the order you will meet them.

### Packages and files

Every directory is a **package**. All `.go` files in a directory start with the same `package xyz` line and share everything, as if they were one big file. Files are split only for readability.

```go
package dhcp            // this file belongs to package "dhcp"

import (
    "context"           // standard library
    "github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript" // our own package
)
```

`internal/` is special: packages under it can only be imported by this module, never by other projects.

The package name is usually the directory name, with one exception here: `internal/resources/dhcp` is `package dhcpresources` and `internal/resources/dns` is `package dnsresources` (same for `dhcpdatasources`, `dnsdatasources`). If they were called `dhcp` and `dns`, they would clash with the client packages they import.

### Capital letter = public

There are no `public`/`private` keywords. A name starting with a **capital letter** is visible outside its package (`dhcp.GetScope`); a lowercase name is private to the package (`scopeFunc`).

### Variables and types

```go
var name string = "Users"   // long form
name := "Users"             // short form: declare and assign, type inferred
name = "Other"              // plain assignment to an existing variable
```

`:=` declares a new variable, `=` changes an existing one. You will see `:=` almost everywhere.

### Structs and struct tags

A **struct** is a record with named fields (like a PowerShell `[pscustomobject]` with a fixed shape):

```go
type Scope struct {
    ScopeID string `json:"scope_id"`
    Name    string `json:"name"`
}
```

The text in backticks is a **tag**: metadata other code reads. `json:"scope_id"` tells the JSON decoder "fill this field from the `scope_id` key". In the resources you will see `tfsdk:"scope_id"`, which tells the Terraform framework "this field is the `scope_id` attribute in HCL".

### Functions, multiple return values and errors

Go functions can return several values. By convention the last one is an `error`:

```go
func (c *Client) GetScope(ctx context.Context, scopeID string) (*Scope, error)
```

There are no exceptions in Go. Every call that can fail returns an error, and the caller checks it right away. This pattern is everywhere:

```go
s, err := c.GetScope(ctx, "10.1.20.0")
if err != nil {
    return err      // give up and pass the error to our caller
}
// here s is valid
```

`nil` means "nothing" (like `$null`). An error that is `nil` means success.

Errors can wrap other errors. `errors.Is(err, dhcp.ErrNotFound)` asks "is this error, or anything it wraps, the not-found error?". The repo uses it to tell "the scope was deleted out of band" apart from "the server is unreachable". (`dhcp.ErrNotFound`, `dns.ErrNotFound` and `psscript.ErrNotFound` are the same value.)

### Methods and receivers

```go
func (c *Client) GetScope(...) ...
```

The `(c *Client)` before the name makes `GetScope` a **method** of the `Client` type, called as `client.GetScope(...)`. `c` plays the role of `this`/`$this`.

### Pointers: `*` and `&`

`*Scope` means "a reference to a Scope" and `&plan` means "a reference to the variable plan". Passing a reference lets the called function modify the original instead of a copy. When you see `resp.State.Set(ctx, &plan)`, read it as "save `plan` into state".

### Interfaces

An **interface** is a list of methods. Any type that has those methods satisfies it automatically; there is no `implements` keyword. The most important one in this repo:

```go
type Runner interface {
    Run(ctx context.Context, script string, params any) ([]byte, error)
}
```

The SSH transport, the WinRM transport, the in-memory fake servers used by tests (`dhcpfake`, `dnsfake`) and the `pwsh` test runner all have a `Run` method with that shape, so the rest of the code can use any of them without knowing which.

You will often see this line:

```go
var _ resource.ResourceWithImportState = &scopeResource{}
```

It does nothing at runtime. It makes the compiler check that `scopeResource` has every method the interface needs (so a missing `ImportState` is a build error, not a runtime surprise). `_` is the "throw this away" name.

### Slices, maps, `any`

- `[]string` is a list of strings (a **slice**). `append(list, x)` adds to it, `len(list)` counts it.
- `map[string]any` is a hashtable with string keys and values of any type. `any` means "any type at all".
- `for i, x := range list { ... }` loops over a slice (index and value); over a map it gives key and value.

### Generics: `[M any]`

`objectSetValues[M any]` (in `internal/resources/dns/record_values.go`) is a **generic** type: one implementation written once, with `M` filled in by each user. `record_mx.go` uses `objectSetValues[mxValue]`, `record_srv.go` uses it with its own SRV struct. Read `[M]` as "for some record shape M".

### `defer`

`defer f()` runs `f()` when the surrounding function returns, whatever the path. It is used for cleanup: `defer sess.Close()`, `defer s.mu.Unlock()`. Think `try { } finally { }`.

### Concurrency: goroutines, channels, mutexes, context

Terraform works on up to 10 resources in parallel, so the provider is called from several threads at once.

- `go f()` starts `f` in the background (a **goroutine**, a lightweight thread).
- A **channel** (`chan T`) is a pipe between goroutines; `select` waits on several channels at once. The runner uses one as a counting semaphore to cap concurrent PowerShell sessions (`max_concurrency`).
- `sync.Mutex` is a lock: `mu.Lock()` / `mu.Unlock()` around code that must not run twice at the same time. The record-set engine keeps one lock per (zone, name, type) so two resources never edit the same DNS name at once.
- `context.Context` (always the first argument, named `ctx`) carries cancellation: when you press Ctrl-C, Terraform cancels the context and long operations stop.

### Tests

Tests live next to the code in files ending in `_test.go`. A test is a function `func TestSomething(t *testing.T)`. `t.Fatal(...)` fails and stops the test, `t.Errorf(...)` fails but continues, `t.Skip(...)` skips it. `go test ./...` runs every test in every package.

### The tools

| Command | What it does |
|---|---|
| `go build ./...` | Compile everything (`./...` means "this directory and all below"). |
| `go test ./...` | Run unit tests. |
| `go vet ./...` | Catch suspicious code. |
| `gofmt -w .` | Format code. Go has one official style; never format by hand. |
| `golangci-lint run` | Many linters at once (config in `.golangci.yml`). |
| `go generate ./...` | Runs the `//go:generate` lines in `main.go`: formats `examples/` and regenerates the Registry docs in `docs/`. |
| `go mod tidy` | Sync `go.mod`/`go.sum` (the dependency list and its checksums) with the imports. |

## 3. Map of the repository

| Path | What lives there |
|---|---|
| `main.go` | Entry point. Starts the gRPC server Terraform talks to. |
| `internal/provider/` | The `provider "windowsddi" {}` block: schema, env var fallback, building the SSH or WinRM connection, registering the DHCP and DNS resources and data sources. |
| `internal/providerdata/` | The `Clients` value (one DHCP client, one DNS client, sharing one connection) the provider hands to every resource, and `DHCPFrom` / `DNSFrom` to pick the right one. |
| `internal/resources/dhcp/` | DHCP resources (`package dhcpresources`), one file per resource: schema plus Create, Read, Update, Delete, ImportState. `common.go` holds the `All()` list. |
| `internal/resources/dns/` | DNS resources (`package dnsresources`): `zone.go`, `conditional_forwarder.go`, and the record sets (`record_base.go` shared implementation, `record_values.go` value codecs, one small `record_*.go` per type). `common.go` holds `All()`. |
| `internal/datasources/dhcp/`, `internal/datasources/dns/` | Data sources, one file each. Schema and Read only. |
| `internal/dhcp/` | Typed DHCP client: `GetScope`, `AddReservation`, `SetOptionValue`... Each file holds the PowerShell for one object type. |
| `internal/dhcp/dhcpfake/` | An in-memory fake DHCP server used by unit tests. |
| `internal/dns/` | Typed DNS client: `GetZone`, `AddPrimaryZone`, `AddConditionalForwarder`, `GetRecords`, `AddRecord`, `RemoveRecord`, `SetRecordTTL`... (`zone.go`, `forwarder.go`, `record.go`). |
| `internal/dns/dnsfake/` | An in-memory fake DNS server used by unit tests. |
| `internal/recordset/` | The record-set engine shared by all DNS record resources: read a (zone, name, type) tuple, create, diff and update, delete, spelling preservation. |
| `internal/psscript/` | Turns a script body plus parameters into the exact text sent to Windows (including the module check), and parses the JSON reply. |
| `internal/runner/` | Transports: SSH and WinRM. Also the concurrency limit. |
| `internal/normalize/` | Parsing and comparing user input: durations (`8h` = `0.08:00:00`), MAC addresses, IPv4, subnet masks, DNS names, IPv6, TTLs, and the custom Terraform types built on them. |
| `internal/acctest/` | Shared test helpers (provider wired to a fake server, or to a real server for acceptance tests). |
| `examples/` | HCL examples. They are copied into the Registry docs. |
| `templates/` | Template for the Registry front page (`docs/index.md`). |
| `docs/` | `DESIGN.md`, `ROADMAP.md`, `LAB_SETUP.md`, `adr/`, this file (hand written); `index.md`, `resources/`, `data-sources/` (generated, do not edit). |
| `.github/workflows/` | CI (`test.yml`) and the signed release on tag (`release.yml`). |
| `.goreleaser.yml` | How release binaries are built, zipped, checksummed and signed. |

## 4. Follow one `terraform apply`

Configuration:

```hcl
resource "windowsddi_dhcp_scope" "users" {
  name           = "Users VLAN 120"
  start_range    = "10.1.20.10"
  end_range      = "10.1.20.250"
  subnet_mask    = "255.255.255.0"
  lease_duration = "8h"
}
```

**1. Terraform starts the provider.** `main.go` calls `providerserver.Serve`, which registers the provider built by `provider.New` and waits for Terraform's calls.

**2. Schemas.** Terraform asks for every schema. `Provider.Schema` (provider block), `scopeResource.Schema` (`internal/resources/dhcp/scope.go`) etc. describe the attributes: which are required, defaults (`state = "Active"`), validators (`IPv4Validator`), plan modifiers (`RequiresReplace` on `subnet_mask`). Terraform validates your HCL against them; that is where "Invalid Duration" or "must be an IPv4 address" errors come from. The list of resources comes from `Provider.Resources`, which joins `dhcpresources.All()` and `dnsresources.All()`.

**3. Configure.** `Provider.Configure` (`internal/provider/provider.go`) reads the provider block, fills gaps from `WINDOWSDDI_*` environment variables (`resolve`), builds an SSH or WinRM runner (`settings.runner`), wraps it in the concurrency limit (`runner.Limit`) and creates a `providerdata.Clients` holding a `dhcp.Client` and a `dns.Client` on that same runner. Nothing connects yet: the SSH connection opens on the first command. Every resource receives the `Clients` value in its `Configure` method and keeps the one it needs: DHCP resources call `clientFrom` (`internal/resources/dhcp/common.go`), which uses `providerdata.DHCPFrom`.

**4. Plan.** Terraform computes the plan. Our hook `scopeResource.ModifyPlan` computes the network address from `start_range` and `subnet_mask` (`normalize.NetworkAddress`), so `scope_id = "10.1.20.0"` shows in the plan instead of "known after apply". It also forces a replacement if you later move `start_range` into another network.

**5. Create.** On apply, Terraform calls `scopeResource.Create`:
   1. `req.Plan.Get(ctx, &plan)` copies the planned values into the `scopeModel` struct.
   2. `plan.input()` converts them to a `dhcp.ScopeInput` (parsing `8h` into a Go `time.Duration`).
   3. `r.client.AddScope(ctx, in)` runs the PowerShell (next section).
   4. `plan.apply(s)` copies what the server returned back into the model.
   5. `resp.State.Set(ctx, &plan)` saves it into the Terraform state.

   The server reports `lease_duration = "0.08:00:00"` but you wrote `8h`. The custom type `normalize.DurationValue` declares the two "semantically equal", so state keeps `8h` and the next plan shows no diff. MAC addresses work the same way (`normalize.MACValue`).

**6. `dhcp.AddScope`** (`internal/dhcp/scope.go`) builds a parameters map (`scope_id`, `name`, `lease_seconds = 28800`, ...) and calls `c.run` with the script `scriptScopeAdd`. That script runs `Add-DhcpServerv4Scope` then reads the scope back with `Get-DhcpServerv4Scope`.

**7. `client.run`** (`internal/dhcp/client.go`) adds `computer_name` if `dhcp_server` is set, attaches the module requirement (`s.Requires = module`, which makes the script check that the `DhcpServer` module is there), logs the call at debug level, and hands the wrapped script and parameters to the runner. `dns.Client.run` (`internal/dns/client.go`) is the same with `dns_server` and the `DnsServer` module.

**8. The runner** (`internal/runner/runner.go`, `execute`) renders the final script (`psscript.Render`), builds the command line and stdin (`psscript.Command`), and asks the transport (`ssh.go` or `winrm.go`) to execute it. It decodes the output and returns the JSON envelope.

**9. Back up the stack.** `psscript.Parse` reads the envelope. `ok: true` gives the scope data; `ok: false` becomes a Go error such as `Add-DhcpServerv4Scope: Access is denied. (PermissionDenied)`, which the resource turns into a Terraform error with `resp.Diagnostics.AddError`.

Read, Update, Delete and Import follow the same path with other scripts (`scriptScopeGet`, `scriptScopeSet`, `scriptScopeRemove`).

DNS zones (`internal/resources/dns/zone.go`) and conditional forwarders (`conditional_forwarder.go`) follow exactly this shape, calling `dns.Client` methods such as `AddPrimaryZone` and `GetZone`. Record sets add one layer; see section 7.

## 5. What actually reaches Windows

For `GetScope("10.1.20.0")`, this is the script the host receives (generated by the code, not edited):

```powershell
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$PSDefaultParameterValues['*:ErrorAction'] = 'Stop'
$p = ConvertFrom-Json -InputObject '{"scope_id":"10.1.20.0"}'
$cn = @{}
if ($p.computer_name) { $cn['ComputerName'] = $p.computer_name }
# windowsddi:scope.get
try {
    $out = $null
    if (-not (Get-Command -Name 'Get-DhcpServerv4Scope' -ErrorAction SilentlyContinue)) { throw 'The DhcpServer PowerShell module is not available on this host. ...' }
    $null = . {

$out = ConvertTo-WdScope (Get-DhcpServerv4Scope @cn -ScopeId $p.scope_id)

    }
    ConvertTo-Json -InputObject @{ ok = $true; data = $out } -Depth 8 -Compress
} catch {
    $e = $_
    ConvertTo-Json -InputObject @{ ok = $false; error = @{
        message  = "$($e.Exception.Message)"
        category = "$($e.CategoryInfo.Category)"
        id       = "$($e.FullyQualifiedErrorId)"
        command  = "$($e.InvocationInfo.MyCommand.Name)"
    } } -Compress
}
```

(The real script also contains the `ConvertTo-WdScope` helper function, which picks the properties to return and formats them as stable strings. The `throw` message ends with a hint naming the role or RSAT feature to install.)

Things to notice:

- **User data only enters through `$p`.** Your values are JSON encoded and placed in one single-quoted string, with quotes doubled. Nothing you type in HCL can become PowerShell code.
- **`$PSDefaultParameterValues['*:ErrorAction'] = 'Stop'`.** The DhcpServer and DnsServer cmdlets are module functions that ignore the caller's `$ErrorActionPreference`. Without this line a "scope not found" would be a non-terminating error and the script would carry on with `$null`. A test (`internal/dhcp/pwsh_test.go`) fails if the line is removed.
- **The module check** (`psscript.Requirement`, written by `Script.Wrap`) runs before the body: a host without the role or RSAT tools gets a clear error instead of "the term ... is not recognized". Its error category is deliberately not `ObjectNotFound`, so it can never be mistaken for "not found" (which would silently drop resources from state).
- **`@cn`** splats `-ComputerName` into every cmdlet when `dhcp_server` (or `dns_server` for DNS scripts) is set (management host setup), and nothing otherwise.
- **`$null = . { ... }`** runs the body in the current scope (so `$out` survives) and throws away anything else it prints, so only the JSON envelope reaches stdout.
- **The envelope** is always one JSON object: `{"ok":true,"data":...}` or `{"ok":false,"error":{...}}`.

How it travels:

```
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand JABQAHIAbwBn...   (about 870 chars, always the same)
stdin:  base64 of the script above (UTF-8)
stdout: base64 of the JSON envelope (UTF-8)
```

The command line is a fixed **bootstrap** (`psscript.bootstrap`): read stdin, base64-decode it, run it, base64-encode the output. This avoids `cmd.exe`'s 8191 character limit (both Windows OpenSSH and WinRM go through `cmd.exe`), avoids every quoting problem, and keeps non-ASCII names intact whatever the console code page.

## 6. Errors, "not found" and drift

- A cmdlet failure comes back as `{"ok":false,"error":{...}}` and becomes a `*psscript.Error`. Its text always starts with the cmdlet name, as required by `CLAUDE.md`.
- Some errors mean "this object does not exist": category `ObjectNotFound`, or a known error code: DHCP `20005` (subnet not present), `20010` (option not present), `20018` (not a reserved client); DNS `WIN32 9601` (zone does not exist), `9701` (record does not exist), `9714` (name does not exist). `psscript.Error.NotFound` recognises them (the list is `notFoundIDs` in `internal/psscript/psscript.go`) and `errors.Is(err, dhcp.ErrNotFound)` / `dns.IsNotFound(err)` is then true.
- Where possible the scripts list and filter instead of relying on error codes (reservations, exclusion ranges, option values, DNS records). An empty result also maps to "not found".
- In every resource's `Read`, not found calls `resp.State.RemoveResource`: Terraform forgets the object and plans to create it again. That is how out-of-band deletions (drift) are handled. In `Delete`, not found is treated as success.

## 7. The DNS side: record sets

Zones and conditional forwarders work like the DHCP resources (section 4). Record sets are different: one resource owns **every** record of one (zone, name, type) tuple, for example all A records of `www` in `example.local`, and it is authoritative (values on the server but not in your configuration are removed). Seven record types share one implementation.

```hcl
resource "windowsddi_dns_a_record_set" "www" {
  zone_name = "example.local"
  name      = "www"
  addresses = ["10.1.20.21", "10.1.20.22"]
}
```

### How a record-set resource flows

```
record_a.go           NewARecordSet: a recordKind (type "A", attribute "addresses", codec stringSetValues)
     │
record_base.go        recordResource: the one Schema/Create/Read/Update/Delete/ImportState for all types
     │                getRecordCommon + kind.values.get  →  recordset.Tuple + []dns.Record
     ▼
internal/recordset    Create / Read / Update / Delete: per-tuple lock, existing-record checks,
     │                add-before-remove diff, TTL changes, PreserveSpelling
     ▼
internal/dns          GetRecords, AddRecord, RemoveRecord, SetRecordTTL (record.go)
     │
     ▼
scripts               dns.record.list / .add / .remove / .set_ttl  →  psscript  →  runner
```

1. **`record_a.go`** (and every other `record_*.go`) is only a declaration: `newRecordResource(recordKind{...})` with the resource name suffix, the record type (`dns.TypeA`), the value attribute name and a **codec** from `record_values.go` that converts that attribute to and from `[]dns.Record`. There are three codecs: `stringSetValues` (A, AAAA, TXT), `hostnameValue` (CNAME, PTR: one target) and `objectSetValues[M]` (MX, SRV: sets of objects).
2. **`record_base.go`** holds the real resource. `Create` reads `zone_name`, `name`, `ttl` and `allow_overwrite_dynamic` (`getRecordCommon`), builds a `recordset.Tuple`, asks the codec for the values, and calls `recordset.Create`. Every method ends with `refresh`, which calls `recordset.Read`, then `recordset.PreserveSpelling`, then writes the result into state with the codec.
3. **`internal/recordset/recordset.go`** is the engine. `Create` fails with an `ExistingRecordsError` when the name already holds records (static: import them; dynamic, meaning registered by a client or the DHCP server: allowed only with `allow_overwrite_dynamic = true`). `Update` diffs against what is on the server now (not the prior state), adds new values before removing old ones so the name never resolves empty, and changes TTLs in place. A lock per tuple (`lock`) serialises operations on the same name.
4. **`internal/dns/record.go`** is the typed client. `dns.Record` is one record with typed fields (`Address`, `HostName`, `Exchange`/`Preference`, `Target`/`Priority`/`Weight`/`Port`, `Text`). `Record.SameValue` is the one definition of "same record" (hostnames ignoring case and the trailing dot, addresses by parsed value) used by the engine, the fake and, mirrored in PowerShell as `Test-WdRecord`, by the scripts.

### DNS script rules

The DNS scripts follow the same contract as section 5, plus a few rules (full list in `docs/DESIGN.md`, "DNS script rules"):

- Record queries always return an array (`$out = @(...)`), so one record does not collapse into an object, and a name with no records returns `[]` rather than an error. `Get-WdRecordSet` reads the zone first so a missing zone still fails with `WIN32 9601` (not found), while `9714` (no such name) becomes an empty list.
- `ConvertTo-WdRecord` projects the CIM `RecordData` explicitly per type (`IPv4Address.IPAddressToString`, `HostNameAlias`, `MailExchange` + `Preference`, ...), emits TTL as whole seconds, and sets `dynamic` from the presence of a `Timestamp`.
- Records are removed by finding the matching CIM object (`Get-WdRecordMatch`) and passing it to `Remove-DnsServerResourceRecord -InputObject ... -Force`, because the `-RecordData` string format differs per type and is ambiguous for MX and SRV. TTL changes clone the object and call `Set-DnsServerResourceRecord -OldInputObject -NewInputObject`.
- `-ReplicationScope` lives in its own parameter set on `Set-DnsServerPrimaryZone` and `Set-DnsServerConditionalForwarderZone`, so zone scripts change it in a separate call, only when it differs.

### Names: semantic equality and spelling preservation

DNS is case insensitive and the server returns names its own way (`Mail.Example.com` comes back as `mail.example.com.`). Two mechanisms keep the plan free of perpetual diffs while state keeps what you wrote:

- **Custom name types** (`internal/normalize/dnstypes.go`): `normalize.ZoneNameType` (`zone_name`, zone and forwarder `name`), `normalize.RecordNameType` (record `name`) and `normalize.HostnameType` (CNAME/PTR `target`). Like `DurationValue` for DHCP, their values implement `StringSemanticEquals`, so `Example.Local` and `example.local.` are "the same" and the framework keeps your spelling. `Canonical()` gives the normalised form (lowercase, trailing dot rules from `normalize.ZoneName`, `normalize.RecordName`, `normalize.FQDN`).
- **Spelling preservation** (`recordset.PreserveSpelling`): set elements (an IPv6 address in `addresses`, the `exchange` inside an MX object) cannot carry a custom type each, so Read does the work: for every record on the server it looks for a prior value (from state, or the plan right after Create/Update) that is `SameValue`, and if found writes that prior spelling back. Records with no match are stored in canonical form (`recordset.Canonical`: lowercase FQDN with trailing dot, compressed IPv6).

A related detail: `recordReplaceIfRenamed` (`record_base.go`) only forces a replacement when `zone_name` or `name` changes meaning, not when only the casing or a trailing dot changes.

## 8. How the tests work

There is no Windows server in CI, so tests are layered:

| Layer | Where | What it proves |
|---|---|---|
| Pure unit tests | `normalize`, `psscript`, `provider` | Parsing, escaping, config resolution, env vars. |
| Recorded fixtures | `internal/dhcp/client_test.go`, `internal/dns/client_test.go` + their `testdata/*.json` | The Go clients send the right parameters and decode real-looking replies. |
| Real PowerShell | `internal/dhcp/pwsh_test.go`, `internal/dns/pwsh_test.go` + `testdata/stubs.ps1` | The generated scripts run under `pwsh`, through the real bootstrap and module check, against stub cmdlets loaded as a module with the documented parameter names. Skipped if `pwsh` is not installed. |
| Transport | `internal/runner/runner_test.go` | A real SSH client against an SSH server started inside the test: auth, host key checks, stdin/stdout, exit codes. |
| Engine | `internal/recordset/recordset_test.go` | Create/Update/Delete against `dnsfake`: existing records, rollback, out-of-band values, TTL disagreement, spelling preservation, the per-tuple lock. |
| End to end | `internal/resources/dhcp/*_test.go`, `internal/resources/dns/*_test.go`, `internal/datasources/*/*_test.go` | Real Terraform runs plan, apply, import, drift and destroy against `dhcpfake` or `dnsfake`, in-memory servers that implement `Runner`, plugged in with `acctest.UnitProviders(runner)`. |
| Acceptance | `TestAcc*` functions | Same configurations against a real server, using `acctest.AccDHCPProviders` / `AccDNSProviders` and checking the server directly with `acctest.AccDHCPClient` / `AccDNSClient`. Skipped unless `TF_ACC=1`, `WINDOWSDDI_USERNAME`/`PASSWORD` and a host are set; `AccPreCheckDHCP` / `AccPreCheckDNS` also skip when the role does not answer, so a DNS-only lab skips the DHCP tests. |

The fakes dispatch on the op marker on the first line of every script (`# windowsddi:dns.record.add`, read with `psscript.Op`), apply the operation to Go maps and answer with the envelope the real script would print, including the real error codes.

Running the acceptance tests against a lab server is the one check that has not happened yet (see `docs/LAB_SETUP.md`).

## 9. Common tasks

**See what the provider does.** `TF_LOG=DEBUG terraform apply` prints every operation with its parameters (`running DHCP script`, `running DNS script`). Passwords are never logged.

**Add an attribute to an existing DHCP resource** (example: scope `max_bootp_clients`):

1. `internal/dhcp/scope.go`: add the field to `Scope` and `ScopeInput`, the parameter to `params()`, the property to `ConvertTo-WdScope`, and pass it to the `Add-`/`Set-` cmdlets in the scripts.
2. `internal/resources/dhcp/scope.go`: add the field to `scopeModel` (with its `tfsdk` tag), the attribute to `Schema` (with a `MarkdownDescription`), and copy it in `input()` and `apply()`.
3. `internal/dhcp/dhcpfake/fake.go` and `internal/dhcp/testdata/stubs.ps1`: teach the fakes the new parameter.
4. Add a test step, run `go test ./...`, `golangci-lint run`, `go generate ./...` (docs).

DNS zones and forwarders work the same way with `internal/dns/zone.go` or `forwarder.go`, `internal/resources/dns/zone.go` or `conditional_forwarder.go`, `dnsfake` and `internal/dns/testdata/stubs.ps1`.

**Add a new resource:** copy the closest existing one (`exclusion_range.go` is the simplest DHCP one, `conditional_forwarder.go` the simplest DNS one), add its scripts in `internal/dhcp` or `internal/dns`, register it in `All()` of its package (`internal/resources/dhcp/common.go`, or `internal/resources/dns/zones.go` / `records.go` which feed `dnsresources.All()`), add an example under `examples/resources/<name>/`, tests, and run `go generate ./...`.

**Add a new DNS record type** (example: CAA):

1. `internal/dns/record.go`: add a `Type...` constant and its `-RRType` spelling in `rrTypeParam`; add data fields to `Record` if none fit; handle the type in `RData`, `SameValue`, `params` and `checkRecord`; project it in `ConvertTo-WdRecord`, compare it in `Test-WdRecord`, and add its `Add-DnsServerResourceRecord...` call to the `switch` in `scriptRecordAdd`.
2. `internal/recordset/recordset.go`: if the data holds a hostname or an address, canonicalise it in `Canonical`.
3. `internal/resources/dns/record_<type>.go`: declare `New...RecordSet` with a `recordKind`, reusing a codec from `record_values.go`; add it to `recordResources()` in `records.go`. No CRUD code to write.
4. `internal/dns/dnsfake/fake.go` and `internal/dns/testdata/stubs.ps1`: teach the fakes the type and the add cmdlet's parameters.
5. Tests: a `recKind` entry in `internal/resources/dns/record_types_test.go`, a case in `internal/dns/pwsh_test.go`; an example under `examples/resources/windowsddi_dns_<type>_record_set/`; the record table in `docs/DESIGN.md`; then `go generate ./...`.

**Run against a lab server** (see `docs/LAB_SETUP.md`):

```sh
export WINDOWSDDI_HOST=ddi-lab.example.local WINDOWSDDI_USERNAME='LAB\admin' WINDOWSDDI_PASSWORD=...
# optional, when the roles live on different servers:
export WINDOWSDDI_DHCP_HOST=dhcp-lab.example.local WINDOWSDDI_DNS_HOST=dc-lab.example.local
# optional, to run AD-integrated zone tests (the DNS host must be a domain controller):
export WINDOWSDDI_AD=1
TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m
```

**Try a local build with real Terraform:** `go build -o ~/tfdev/terraform-provider-windowsddi .`, then in `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides { "thomaschristory/windowsddi" = "/Users/<you>/tfdev" }
  direct {}
}
```

and run `terraform plan` in any directory using the provider (no `terraform init` needed).

## 10. Glossary

| Term | Meaning here |
|---|---|
| **Framework** | `terraform-plugin-framework`, HashiCorp's Go library for writing providers. Gives us `schema`, `types.String`, plan modifiers, diagnostics. |
| **Model** | A Go struct mirroring a resource's attributes (`scopeModel`). The framework copies plan/state in and out of it. (Record-set resources read attributes one by one instead, because their value attribute differs per type.) |
| `types.String` | A string that can also be null (not set) or unknown (known after apply). Plain Go strings cannot express that. |
| **Plan modifier** | A rule run during plan: `RequiresReplace` (change forces destroy/create), `UseStateForUnknown` (keep the old value instead of "known after apply"). |
| **Diagnostics** | How errors and warnings reach the Terraform user (`resp.Diagnostics.AddError`). |
| **Semantic equality** | "Different text, same meaning" (`8h` vs `0.08:00:00`, `Example.Local` vs `example.local.`): the framework keeps your spelling and shows no diff. |
| **Spelling preservation** | The record-set equivalent for set elements: Read writes back the prior spelling of every value the server still holds (`recordset.PreserveSpelling`). |
| **Envelope** | The JSON object every script prints: `{ok, data, error}`. |
| **Op** | The name on the first line of a script (`# windowsddi:scope.get`, `# windowsddi:dns.record.add`). Test fakes use it to know which operation was called. |
| **Requirement** | The module check a client attaches to each script (`psscript.Requirement`): a probe cmdlet that must exist, or the script fails with a hint. |
| **Runner** | Anything that can run a script: SSH, WinRM, the fake servers, the `pwsh` test runner. |
| **Tuple** | A (zone, name, type) triple, the unit a record-set resource owns (`recordset.Tuple`). |
| **Dynamic record** | A DNS record registered by a client or the DHCP server (it has a timestamp). Record sets refuse to take them over unless `allow_overwrite_dynamic = true`. |
| **Drift** | The server no longer matches state (someone changed it by hand). Read detects it; the next plan corrects it. |
