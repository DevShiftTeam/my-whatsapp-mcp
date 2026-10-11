# Vendored SQLite components

sqlite-vec.c and sqlite-vec.h are the stable 0.1.9 release amalgamation,
https://github.com/asg017/sqlite-vec/releases/download/v0.1.9/sqlite-vec-0.1.9-amalgamation.tar.gz
SHA256 3acd67cb4aff080c7050926fd3cf8227905fe5b7ee3829d8ee5024ab1283cf61.
Upstream dual MIT/Apache-2.0 licenses are included.

sqlite3.h is copied byte for byte from github.com/mattn/go-sqlite3 v1.14.52
sqlite3-binding.h, matching the pinned driver's bundled amalgamation. SQLite
is public domain; header copyright/disclaimer is preserved. Only sqlite-vec
is compiled in this package; SQLite is compiled by the driver.
