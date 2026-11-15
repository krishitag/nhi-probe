"""Cloud agent for the nhi-probe lab: Google ADK, Gemini model (free API key).

Two agents:  supervisor  --transfer-->  coder  --tool-->  GitHub REST API
So one run gives an agent-to-agent hop AND a credentialed tool call.

Credentials come from ~/nhi-lab/adk/.env (ADK's usual convention, plaintext):
  GOOGLE_API_KEY  - Gemini key (C3)
  GITHUB_TOKEN    - my lab GitHub PAT (C1, reused on purpose: "one NHI, 3+ agents")

Every ADK event is appended, unchanged, to ~/nhi-lab/adk-trace.jsonl as
{"session_id": ..., "event": <ADK event>}. That file is trace's cloud source.
The token is never written there: events only hold tool names, args and results.

Run:  ~/nhi-lab/adk/.venv/bin/python lab/adk_lab.py "List the open issues in krishitag/nhi-lab-sandbox"
"""

import asyncio
import json
import os
import pathlib
import sys
import urllib.request

LAB = pathlib.Path.home() / "nhi-lab"
ENV_FILE = LAB / "adk" / ".env"
TRACE_FILE = LAB / "adk-trace.jsonl"
MODEL = os.environ.get("ADK_MODEL", "gemini-3.8-flash")  # 2.5-flash was retired for new users (404 on 15 Nov 2026)


def load_env(path):
    """Tiny .env reader: KEY=VALUE lines, # comments. Never prints values."""
    for line in path.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


load_env(ENV_FILE)

from google.adk.agents import Agent  # noqa: E402  (needs GOOGLE_API_KEY loaded first)
from google.adk.runners import Runner  # noqa: E402
from google.adk.sessions import InMemorySessionService  # noqa: E402
from google.genai import types  # noqa: E402


def github_list_issues(owner: str, repo: str) -> dict:
    """List the open issues of a GitHub repository (read-only)."""
    req = urllib.request.Request(
        f"https://api.github.com/repos/{owner}/{repo}/issues?state=open",
        headers={
            "Authorization": "Bearer " + os.environ["GITHUB_TOKEN"],
            "Accept": "application/vnd.github+json",
        },
    )
    with urllib.request.urlopen(req, timeout=20) as resp:
        data = json.load(resp)
    return {"issues": [{"number": i["number"], "title": i["title"]} for i in data]}


coder = Agent(
    name="coder",
    model=MODEL,
    description="Answers questions about GitHub repositories using the github_list_issues tool.",
    instruction="Use github_list_issues to answer questions about GitHub issues. Be brief.",
    tools=[github_list_issues],
)

supervisor = Agent(
    name="supervisor",
    model=MODEL,
    description="Routes each request to the right specialist agent.",
    instruction="You are a supervisor. For anything about GitHub, transfer to the coder agent.",
    sub_agents=[coder],
)


async def main(prompt):
    sessions = InMemorySessionService()
    session = await sessions.create_session(app_name="nhi_lab", user_id="lab-user")
    runner = Runner(agent=supervisor, app_name="nhi_lab", session_service=sessions)
    message = types.Content(role="user", parts=[types.Part(text=prompt)])

    with TRACE_FILE.open("a") as trace:
        async for ev in runner.run_async(user_id="lab-user", session_id=session.id, new_message=message):
            record = {"session_id": session.id, "event": json.loads(ev.model_dump_json(exclude_none=True))}
            trace.write(json.dumps(record) + "\n")
            if ev.content and ev.content.parts:
                for part in ev.content.parts:
                    if part.text:
                        print(f"[{ev.author}] {part.text}")
                    if part.function_call:
                        print(f"[{ev.author}] calls {part.function_call.name}")
    print(f"\ntrace appended to {TRACE_FILE}")


if __name__ == "__main__":
    asyncio.run(main(" ".join(sys.argv[1:]) or "List the open issues in krishitag/nhi-lab-sandbox"))
