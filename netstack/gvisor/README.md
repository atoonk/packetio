# gvisor - the patched netstack this module runs on

Upstream [gVisor](https://github.com/google/gvisor) at the commit in `PIN`,
with the patches in `patches/` applied, trimmed to the packages
`netstack` imports (and what those import, under every build tag), import
paths rewritten from `gvisor.dev/gvisor/` to
`github.com/atoonk/packetio/netstack/gvisor/`. Tests, BUILD files and
everything outside the closure are left out. `LICENSE` and `AUTHORS` are
upstream's; the code is Apache 2.0.

The patches are go-afxdp's `examples/netstack/patches`, plus the ones review
here has added, kept as a git history and exported with `git format-patch`.
In order:

| | what it changes |
|---|---|
| 0001 | delayed ACK: a reply carries the ACK instead of a bare ACK preceding it |
| 0002 | an uncontended connection's segments are processed on the goroutine that delivered them; the rest is deferred to `DrainDeferred` |
| 0003 | a pooled buffer chunk is not zeroed on reuse |
| 0004 | views over caller-owned memory, for endpoints that hand the stack their own frames |
| 0005 | a segment shares the delivered packet instead of cloning it |
| 0006 | a packet's data is read without cloning its views |
| 0007 | segments queued for reassembly are counted |
| 0008 | link backpressure (`ErrWouldBlock` from the link pauses the sender), and a zero-window probe that is not counted as a loss |
| 0009 | floors under the tail loss probe timeout and the RACK reorder window; no probe while only the peer's window holds the sender back |
| 0010 | trims on the connection-churn path |
| 0011 | an ARP request sends the target link address as zero rather than whatever the recycled buffer held (0003 leaves it unzeroed) |
| 0012 | a static entry for an address being resolved completes that resolution, so the packets queued behind it are sent instead of dropped |
| 0013 | the header room a packet buffer reserves is zeroed, so a field a protocol leaves unwritten (ICMPv4's unused byte) cannot carry recycled memory onto the wire; 0003 stopped zeroing chunks, and this is the header, not the data |

Every knob the patches add is package-level and off by default, so the
tree behaves as upstream until `netstack.New` turns them on (`../fork.go`).

## Regenerating

`pkg/` is generated and never edited by hand. To move the pin or change a
patch: edit `PIN` (the upstream commit; the pseudo-version after it is a
record of what `go get gvisor.dev/gvisor` would call the same commit, which
nothing reads) or `patches/`, then

```bash
./import.sh          # fetches the pin from GitHub, applies the patches, rewrites pkg/
./import.sh -check   # regenerates into a scratch dir and diffs against pkg/ (CI runs this)
./import.sh -src /path/to/gvisor/checkout    # offline, from a clone that has the pinned commit
```

The script refuses to overwrite a `pkg/` with uncommitted changes, and fails
if the patched tree is not gofmt-clean before the path rewrite (which moves
import alignment; gofmt puts it back). The closure is computed from the
imports in the `netstack` sources, so a new gVisor package used by the stack
is picked up on the next run.
