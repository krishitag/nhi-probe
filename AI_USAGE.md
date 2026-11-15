# AI usage

I used Claude Code (Anthropic) as a tutor and pair programmer while learning Go. Exactly what it did:

- **Planning:** broke the assignment into blocks and steps.
- **First part (tutor mode):** Claude drafted each Go file (fingerprint, detect, the stdio proxy with `kestrelShard` and credential fingerprints, the bounded event writer, Ctrl-C / exit 17, scan), explained it in simple words, and quizzed me on it. I reviewed, ran and tested each one myself.
- **Second part (time pressure):** Claude wrote the rest (hook logger, trace with its adapters, pipeline, graph, risks and lineage, the ADK adapter and lab script, the Streamable HTTP proxy, the replay/loadgen tools, the scaling fixes and the scale scripts) and wrote down every design choice for me to review.
- **Docs:** Claude drafted DOSSIER.md, SCALING.md and README.md from my lab results and measurements. I checked them against my lab.
- **Me:** all lab setup (GitHub / Notion / Gemini credentials with least privilege, MCP wiring, hooks, the ADK agent), every command, every test and scale run, reading the profiles, every commit, and a code walkthrough with Claude so I can explain each part and each goroutine.
