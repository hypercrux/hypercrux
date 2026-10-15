# Notes for Claude

Releases are automated. To release, raise the version and push to `main`:
`Version` in `hypercrux.go` for 0.x, or in `beta/hypercrux/hypercrux.go`
for the Beta, which goes out as a pre-release. Never ask the owner to
publish one. [RELEASING.md](RELEASING.md) has the whole procedure, the
checks to run afterwards, and how to copy it to a new project.

The Beta was built in rounds from the plan in [BETA.md](BETA.md), and every
task in it is done. Its first release, 0.3.0-beta.1, went out on 15 October
2026, and the task board, [beta/tasks/README.md](beta/tasks/README.md),
records each task. What comes next hasn't been decided: agree it with the
owner before it goes here, and start work only when the owner says go.
