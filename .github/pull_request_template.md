<!--
One issue per PR. Link it below so merging closes it automatically.
-->

## Summary

<!-- One line: what changed and why. Not a diff summary. -->

Closes #

## Tradeoff / most likely to be wrong

<!--
Name the main tradeoff you made, or the part of this change you'd most want a
reviewer to look at hardest. "Nothing" is rarely true.
-->

## Invented, not specified

<!--
Anything you decided that the issue didn't: role names, API shapes, defaults,
file paths, env var names. These harden into later work, so flag them here for
a human decision now. Write "none" if the issue specified everything.
-->

## Definition of done

From `~/.claude/CLAUDE.md` — "I'll add tests later" is not done.

- [ ] Lint clean (`golangci-lint run` for `apps/api`, `npm run lint` for `apps/web`)
- [ ] Tests passing locally, and new tests cover the behaviour this PR adds
- [ ] Structured logs/metrics added, if this touches a request path or adds an
      external call (`Standards/Observability Standards.md`)
- [ ] No secrets in code, committed env files, or hardcoded config
- [ ] Anything noticed but out of scope was **filed as an issue**, not folded
      into this PR — linked below

## ADR

`Standards/CI-CD Standards.md` asks this on every PR:

- [ ] **Did this change need a new ADR?** A decision that's expensive to reverse
      — a datastore, a messaging pattern, a cloud service chosen over an
      alternative, a tenancy or auth model — goes in `docs/adr/`, not just in the
      PR description. Tick this once you've considered it, and say "no ADR
      needed" here if that's the answer.

## Related issues filed from this work

<!-- e.g. "Filed #12 — noticed while implementing this". Write "none" if none. -->

---

### Required status checks

Branch protection on `main` requires the **`ci`** check.

`ci` is an aggregate: it always runs, and it passes only if every applicable job
(`api`, `web`) passed. `api` and `web` are skipped for changes that don't touch
their app, which is why the required check is `ci` and not the app jobs
themselves — a skipped job can leave a required check waiting forever. Don't add
`api` or `web` as required checks directly.
