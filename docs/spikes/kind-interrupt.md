# Spike: Kind interrupt and cleanup

Date: 2026-10-31. A throwaway Go program (not in this repository), 8 runs on a Windows 11 + WSL2 laptop with about 3.8 GB of memory available to Docker. These are observations, not final design numbers.

## Findings

1. **Child in its own process group, parent forwards SIGINT.** 5 of 5 interrupts (from 10 s to 118 s into `kind create`) ended with exit 130 and no leftover cluster.
2. **Child sharing the terminal's process group.** The child died first and the run was classified `errored` (exit 2) instead of `canceled` (130) in the one interrupt tested. A signal must be recorded by the signal handler itself, never inferred from context state or from the child's exit.
3. **Late node container.** In one run Docker created the node container after kind had been interrupted and after `kind delete` had returned. A single check right after delete would have reported clean. Verifying for a window and retrying delete once found and removed it.
4. **Timing on this laptop** (one successful create): create about 240 s, delete 65 to 80 s. A 5-minute provision deadline and a 90-second cleanup timeout are too tight here. Defaults must be higher and configurable; exact numbers need more data.

## Design implications

- Run external tools in their own process group, forward SIGINT, and escalate to kill after a grace period.
- Wind-down (delete and verify) runs on a detached context with its own, generous budget.
- Cleanup verification uses a window plus one retry, and a leaked cluster is recorded in the report.
- Cleanup problems never change the outcome (section 6.4 of the architecture overview).

## Not tested

A second Ctrl-C during cleanup, SIGTERM, Ctrl-C while idle after create, Ctrl-C in the first seconds, and Docker with more memory.