"""Copy my real lab data into captures/, keeping only what trace needs.

- proxy event files: copied as-is (they already hold only fingerprints + first4...last4)
- adk-trace.jsonl:   copied, minus model "thought_signature" blobs and tool results (issue titles etc.)
- hooks.jsonl:       only the fields the hooks adapter reads (no prompts, tool inputs/outputs, file paths)

After running, check with:  ./nhi-probe scan --agents=false --out /tmp/captures-scan.json captures
It must report 0 secrets.
"""

import json
import pathlib
import shutil

LAB = pathlib.Path.home() / "nhi-lab"
OUT = pathlib.Path(__file__).resolve().parent.parent / "captures"
OUT.mkdir(exist_ok=True)

for f in sorted(LAB.glob("*-events.jsonl")):
    shutil.copy(f, OUT / f.name)
    print("copied", f.name)

HOOK_FIELDS = {"nhi_ts", "hook_event_name", "session_id", "tool_name", "tool_use_id", "agent_id", "agent_type"}
with open(LAB / "hooks.jsonl") as src, open(OUT / "hooks.jsonl", "w") as dst:
    n = 0
    for line in src:
        try:
            d = json.loads(line)
        except json.JSONDecodeError:
            continue
        dst.write(json.dumps({k: v for k, v in d.items() if k in HOOK_FIELDS}, sort_keys=True) + "\n")
        n += 1
    print("hooks.jsonl:", n, "lines (reduced fields)")


def strip(obj):
    """Remove thought signatures and tool results, keep structure."""
    if isinstance(obj, dict):
        out = {}
        for k, v in obj.items():
            if k == "thought_signature":
                continue
            if k == "function_response":
                out[k] = {"id": v.get("id"), "name": v.get("name")}
                continue
            if k == "text":
                out[k] = "<text removed>"
                continue
            out[k] = strip(v)
        return out
    if isinstance(obj, list):
        return [strip(x) for x in obj]
    return obj


adk = LAB / "adk-trace.jsonl"
if adk.exists():
    with open(adk) as src, open(OUT / "adk-trace.jsonl", "w") as dst:
        for line in src:
            dst.write(json.dumps(strip(json.loads(line))) + "\n")
    print("adk-trace.jsonl: copied (stripped)")
