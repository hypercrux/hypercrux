# Saved sequences

When the differential harness finds two engines disagreeing, it shrinks the
sequence of steps as far as it will go, then saves it here as
`failing-<hash>.json`. `TestSavedSequences` replays every file in this
folder, so a sequence stays as a test after the difference it found is
fixed.

Each file names the two engines, the seed the sequence came from, the
mismatch as it was first reported, and the steps. Values carry their type,
with floats written by their bits.
