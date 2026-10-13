# Saved sequences

When the differential harness finds two engines disagreeing, it shrinks the
sequence of steps as far as it will go, then saves it here as
`failing-<hash>.json`. `TestSavedSequences` replays every file in this
folder with 0.x against the Beta, so a sequence stays as a test after the
difference it found is fixed. A sequence with a step the Beta can't take
yet replays with 0.x against itself; since task G4 there's no such step.

Each file names the two engines, the seed the sequence came from, the
mismatch as it was first reported, and the steps. Values carry their type,
with floats written by their bits.

`failing-e8797536e120.json` is the first the Beta met, in task G2: a Put
that fails inside an Update leaves its new fields in 0.x, spelt its way,
and nothing in the Beta, on purpose. The harness has compared field names
regardless of case since then.

Since G4 the sequences run SQL too, and a Put that fails on its vector's
size inside an Update that commits would leave fields in 0.x that SQL can
name and the Beta hasn't got. The generator keeps that out: it knows each
table's vector size, and gives a vector it knows to be the wrong size a
Put of its own. `TestTheGeneratorKnowsEachTablesSize` holds what it knows
to 0.x.
