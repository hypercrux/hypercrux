# Saved sequences

When the differential harness finds two engines disagreeing, it shrinks the
sequence of steps as far as it will go, then saves it here as
`failing-<hash>.json`. `TestSavedSequences` replays every file in this
folder with 0.x against the Beta, so a sequence stays as a test after the
difference it found is fixed. A sequence with SQL or a filtered search in
it replays with 0.x against itself until task G4.

Each file names the two engines, the seed the sequence came from, the
mismatch as it was first reported, and the steps. Values carry their type,
with floats written by their bits.

`failing-e8797536e120.json` is the first the Beta met, in task G2: a Put
that fails inside an Update leaves its new fields in 0.x, spelt its way,
and nothing in the Beta, on purpose. The harness has compared field names
regardless of case since then.
