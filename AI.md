# How AI was used

Vedetta is made by one person with AI coding tools. This page says which tools, for
what, and how much. The parts and the levels follow
[ai-declaration.md](https://ai-declaration.md/).

## Who does what

The author decides what Vedetta is and does: the method (the model searches and reads,
Vedetta checks what can be checked and says where each sentence comes from), the rules
it follows, what goes in and what stays out, and every release. The author uses Vedetta
every day and reads real sessions one by one to find what goes wrong.

The code, the tests, the measurement scripts and most of the text are written by AI
models, from the author's instructions, and checked by the author and by other models.

## Tools

| Tool | Models | When |
|---|---|---|
| OpenAI Codex | the GPT-5 models of the time | until August 2026 |
| Google Antigravity | Gemini (3.8 Flash in the last months) | from the start to November 2026 |
| pi, a coding agent | DeepSeek V4.1 Flash; at home DeepSeek V4 Flash and Qwen 3.8 Flash Next | September to November 2026 |
| Claude Code | Claude Opus 5.5; for a few steps Claude Fable 5.2 and Claude Sonnet 5.5 | from October 2026 |
| Local models, at home | — | some reviews |

## How much

| Part | Level | What it means here |
|---|---|---|
| Design | Pair | The author sets the method, the rules and the choices; the models propose how to build them, and the author picks. |
| Implementation | Generated | The author says what is wanted, the models write the code. |
| Testing | Generated | The models write the tests and the measurement scripts. The author tries the changes on real questions and decides what the measurements mean. |
| Documentation | Generated | The models write the drafts from the author's notes and rewrite them on the author's corrections, until the author approves them. The English text is a translation by a model. |
| Review | Generated | Reviews of the code by models different from the one that wrote it; the author decides what to fix. |
| Deployment | Generated | The models write the build, the release workflow and the checks on GitHub; tags and releases are done by the author. |

Levels: *Pair* is about half made by a person and half generated; *Generated* is
written by a model from a person's instructions.
