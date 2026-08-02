# Distribution artifacts — prepared, NOT submitted

Submission-ready content for three upstream listings, plus the logo asset one of them
requires.

**Nothing here has been submitted, and nothing here should be submitted automatically.**
Each of these is a public action under the maintainer's name, in someone else's
repository. They are staged so that submitting is a review-and-paste, not a research
exercise.

## Status at a glance

| Listing | State | Blocker |
|---|---|---|
| [last9/awesome-sre-agents](awesome-sre-agents.md) | ✅ **Already listed** | None — nothing to do |
| [cncf/landscape](cncf-landscape.md) | 🚫 **Blocked** | 300-star minimum; RunLore has 13 |
| [Artifact Hub](artifact-hub.md) | 🟢 **Ready** | None — no maturity gate |

Only Artifact Hub is actionable today.

## The logo

[`runlore.svg`](runlore.svg) is the stacked mark-over-wordmark variant CNCF asks for,
with all text converted to outlines. [`make-logo.py`](make-logo.py) regenerates it.

It exists because `assets/logo-wordmark.svg` cannot be submitted as-is, and the reason
is worth recording: it contains a live `<text>` element requesting
`font-family: 'Space Grotesk'`, but the font's actual family name is
**"Space Grotesk SemiBold"**. The request therefore never matches, and fontconfig
silently falls back — on the machine this was prepared on, to Noto Sans:

```console
$ fc-match "Space Grotesk:weight=demibold"
NotoSans-Bold.ttf: "Noto Sans" "Bold"
```

So the shipped wordmark renders in a different typeface almost everywhere, including in
any browser without the font. That is precisely the failure mode the outline requirement
exists to prevent. `runlore.svg` renders identically everywhere because it carries no
font dependency at all.

Verified on the generated file: zero `<text>` elements, zero `<image>` elements, pure
path geometry, 3.2 KB.
