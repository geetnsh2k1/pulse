# pulse starter: node-api

One Node.js function behind `GET /hello`. The smallest possible pulse app —
no queues, no tables, no schema: `resources` is only ever needed for what
you actually use.

```bash
pulse start
curl "localhost:3000/hello?name=you"
```

Then make it yours: edit `functions/hello/index.mjs` and save — the next
request runs your new code. Grow it with `pulse add`:

```bash
pulse add route POST /hello --function hello
pulse add function goodbye
pulse add queue jobs --worker goodbye
```

Handy while developing: `pulse logs hello -f` (live logs) ·
`pulse invoke hello --event events/hello.json` (run without HTTP) ·
`pulse list` (see everything).
