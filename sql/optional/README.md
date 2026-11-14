# Optional SQL

Scripts that are not part of a database load. Nothing here is executed by the
Docker first-run import (which loads only the files directly in `sql/`) or by the
manual load loop in the top-level README.

- `perf_indexes.sql`, `perf_indexes2.sql`: the performance indexes, as a readable
  record. The server applies the same statements itself at boot
  (`db/perfindex.go`, `EnsurePerfIndexes`). They alter `ramJobs` and
  `ramassemblylines`, whose dumps sort after `p`, so loading them in alphabetical
  order with the dumps fails on a fresh database. Load them by hand only against a
  database that already holds every table.
