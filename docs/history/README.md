# History

This repository held the Topos runtime, an embeddable Go library, through
v0.7.0. The tree was cleared and restarted as the Topos core on 2026-09-26.

The v0.7.0 tree is at
[github.com/latere-ai/topos/tree/v0.7.0](https://github.com/latere-ai/topos/tree/v0.7.0),
and every tag from v0.0.2 to v0.7.0 still resolves through the Go module
proxy, so a program pinned to one of them keeps building.

`specs/` here holds that runtime's specs, unchanged: 001 to 012 and 022 to
032 as they stood at the root, and 013 to 018 from its `.archive/`. They are
a record and describe no current behavior. A spec of the new core that
borrows an idea from one of them cites it as "v0.7.0 spec NNN".

This directory is removed in one commit when the rebuild closes, and the
v0.7.0 tag is the record from then on.
