# AGENTS.md

## UX rule

Zero-knowledge, zero-memory UX. The owner should only need to vaguely remember the app's name. Canonical copy: `.cursor/rules/zero-knowledge-ux.mdc`.

- Never require the user to set environment variables (like PYTHONPATH), run `python -m ...`, or type CLI flags. Those belong only in a clearly labeled Developer section.
- Provide obvious launchers: a double-clickable .bat on Windows and an executable script on Linux/macOS. They set paths internally, open the app or its browser page, and show plain-language errors that say what to do next.
- Put every operational launcher in one standard `scripts/` directory. Put developer, test, CI, and maintenance scripts elsewhere (e.g. `tools/dev/`) so `scripts/` stays uncluttered. Keep `scripts/README.md` current.
- Auto-detect configuration. Ask only what cannot be detected, in plain words, with a default. Hide advanced options under Advanced.
- As the project matures, make it GUI-like and add a desktop/Start-menu shortcut named after the app.
- Add a test that fails if user-facing docs contain PYTHONPATH or `python -m` outside the Developer section (when the repo has tests).
- Do not explain Python or tooling internals to the user; just make it work.
