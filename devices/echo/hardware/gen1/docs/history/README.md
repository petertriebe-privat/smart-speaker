# Bring-up history (Echo Gen 1)

These are the dated bring-up notes of the first Echo Gen 1 units, translated to English from the
original German working notes. They record what was tried in which order, including dead ends and
intermediate states that later notes supersede (for example the "reset after 56 s" problem, solved
by the watchdog keep-alive, or the SD-boot path, replaced by the internal start).

**The topic documents in [`..`](..) are current**: [hardware](../hardware.md),
[boot-and-root](../boot-and-root.md), [recovery](../recovery.md), [network](../network.md),
[audio](../audio.md), [known-issues](../known-issues.md). Use these notes only for the original
context, dates and raw measurements.

Statements such as "not yet tested" or "still open" inside these files describe the state on the
date of the note, not today. Device names, COM ports, disk numbers, log file names and `local/`
paths refer to the original private setup.
