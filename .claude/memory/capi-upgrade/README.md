# CAPx upgrade — this fork's memory (read this first)

**MAAS is spectro-OWNED — there is no upstream to reconcile.** This fork does not run the
reconcile engine or the Gate-1 decisions flow; it uses the `capi-maas-release` skill
(bump → regenerate → release). So this memory has **no `decisions/` folder** — only config,
notes, and generated records. Engine, skills, and agents come from the `spectro-capx-upgrade`
plugin, not from here.

## Layout
.claude/memory/capi-upgrade/
├── conf/       — repo config set ONCE (committed): PROVIDER · SPECTRO_SRC · NO_UPSTREAM=1 · CLOUD · CBT_FEATURE · VENDORCRD · BUILD_VARS_*
├── notes/      — freeform notes / parity write-ups (optional)
└── runtime/    — engine/release-generated records (generated for you)
> `UPSTREAM_URL` empty + `NO_UPSTREAM=1` in conf = the MAAS variant.
