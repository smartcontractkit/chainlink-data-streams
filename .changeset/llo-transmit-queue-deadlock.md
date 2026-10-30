---
"@chainlink/data-streams": patch
---

Fixed a Postgres deadlock (`40P01`) between `Delete` and `Prune` on `llo_mercury_transmit_queue`. `Prune` now selects candidate rows with `FOR UPDATE SKIP LOCKED`, so it no longer holds locks in an order that can conflict with `Delete`. A skipped row is left for the next prune cycle.
