# msg

A small, self-hosted, web based group chat written in Go: a single CGO free
binary with SQLite storage, passkey only sign in, server rendered HTML and very
little JavaScript. Run it in development with `DEV=1 go run .` and open
http://localhost:8080 (configuration is read from the environment or a `.env`
file). Enable the git hooks after checkout with
`git config core.hooksPath .hooks/`.
