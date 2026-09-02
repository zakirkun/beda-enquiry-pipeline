#!/usr/bin/env python3
"""Minimal stand-in for the OpenAI and Anthropic APIs, for end-to-end smoke
testing without spending money or needing keys.

It is NOT part of the product and is not imported by anything in internal/.
It answers /v1/chat/completions and /v1/messages with a fixed tool call whose
grounding quotes are copied out of the enquiry text it is given, so the real
grounding check in internal/llm/extract.go does actual work against it.

  python scripts/mock_llm.py 9101 &
  OPENAI_BASE_URL=http://localhost:9101/v1 ANTHROPIC_BASE_URL=http://localhost:9101 ./api
"""
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def classify(text: str) -> dict:
    low = text.lower()
    quotes: dict[str, str] = {}

    def grab(pattern: str) -> str | None:
        m = re.search(pattern, text, re.I)
        return m.group(0) if m else None

    email = grab(r"[\w.+-]+@[\w-]+\.[\w.-]+")
    phone = grab(r"\b0\d{2,3}[\s-]?\d{3,4}[\s-]?\d{3,4}\b")
    name = grab(r"(?:I am|I'm|Name:)\s*[A-Z][a-z]+ [A-Z][a-z]+")
    company = grab(r"(?:at|from|Company:)\s+[A-Z][\w&.-]*(?: [A-Z][\w&.-]*){0,3}(?: Ltd| Retail| Inc)?")

    if any(w in low for w in ("seo", "backlink", "guest post", "claim your discount")):
        return {
            "enquiry_type": "junk", "confidence": 0.96, "urgency": "low",
            "intent_summary": "Unsolicited SEO/backlink marketing.",
            "contact": {"name": None, "email": None, "phone": None, "company_name": None},
            "missing_fields": [], "grounding_quotes": {"enquiry_type": grab(r"[^.\n]*backlink[^.\n]*") or ""},
        }

    words = len(text.split())
    if words < 12:
        return {
            "enquiry_type": "insufficient_info", "confidence": 0.88, "urgency": "low",
            "intent_summary": "Asks about bulk discounts but gives no detail or contact info.",
            "contact": {"name": None, "email": None, "phone": None, "company_name": None},
            "missing_fields": ["name", "email", "quantity"], "grounding_quotes": {},
        }

    support = any(w in low for w in ("error", "not working", "down", "broken", "502", "blocking"))
    urgent = any(w in low for w in ("urgent", "before friday", "blocking", "down", "asap"))

    for key, val in (("email", email), ("phone", phone), ("name", name), ("company_name", company)):
        if val:
            quotes[key] = val
    # A classification quote must come from the enquiry, not the prompt.
    quotes["enquiry_type"] = grab(r"[^.\n]*(?:units|pricing|quote|error|down|order|roles|earnings|setters|recordings)[^.\n]*") or ""
    if urgent:
        quotes["urgency"] = grab(r"[^.\n]*(?:urgent|Friday|blocking|down)[^.\n]*") or ""

    name_val = re.sub(r"^(?:I am|I'm|Name:)\s*", "", name, flags=re.I) if name else None
    company_val = re.sub(r"^(?:at|from|Company:)\s+", "", company, flags=re.I) if company else None

    return {
        "enquiry_type": "support" if support else "sales",
        "confidence": 0.91,
        "urgency": "high" if urgent else "medium",
        "intent_summary": ("Reports a production problem and wants it fixed."
                          if support else "Wants pricing and lead times for a bulk order."),
        "contact": {"name": name_val, "email": email, "phone": phone, "company_name": company_val},
        "missing_fields": [],
        "grounding_quotes": quotes,
    }


DRAFT = ("Thanks for getting in touch about this.\n\n"
         "I've passed the details to the right person on our team and they will follow up "
         "with the specifics shortly.\n\nThe BEDA Team")

# The simulator (internal/llm/simulate.go) asks for a JSON enquiry payload on the
# same no-tools path as drafting, so the mock has to tell the two apart. It keys
# on the "Channel: <name>" line the simulator sends, and returns a payload whose
# details vary per call so dedupe does not collapse repeated clicks.
SIM_CHANNEL = re.compile(r"^Channel:\s*(\w+)", re.M)

def simulated_payload(channel: str, brief: str, seed: int) -> dict:
    """One payload per scenario. The channel comes from the simulator's header line;
    the three email scenarios are told apart by a keyword in the brief. Each scenario
    gets its own person and company: sharing one would make the fuzzy contact matcher
    flag every enquiry after the first as an ambiguous match."""
    phone = f"04{seed % 100:02d} 946 {seed % 1000:03d}"

    if channel == "messaging":
        return {"sender_handle": f"@drew_{seed}", "text": "hey is there anything going in Bali now?"}

    if channel == "web_form":
        addr = f"priya.raman{seed}@lumaflow{seed}.example"
        return {
            "name": "Priya Raman", "email": addr, "phone": phone,
            "company": "Lumaflow Media",
            "message": ("I am Priya Raman, currently a closer at Lumaflow Media in Melbourne. "
                        "I want to know what sales roles are open in Bali, what the on-target earnings "
                        "look like, and what relocation involves. Three years of closing experience, "
                        f"my number is {phone}."),
        }

    if "backlink" in brief:
        return {
            "from": f"Growth Team <offers{seed}@rankfast.example>",
            "subject": f"Guaranteed first page for wearebeda.com ({seed})",
            "body": ("Hi,\n\nWe build high authority backlinks and guest posts on DA60+ sites. "
                     "See https://rankfast.example/proof and https://rankfast.example/pricing.\n\n"
                     f"Claim your discount before the end of the month, ref {seed}."),
        }

    if "broken" in brief:
        addr = f"marcus.hale{seed}@brightpath{seed}.example"
        return {
            "from": f"Marcus Hale <{addr}>",
            "subject": f"Call recordings not syncing to our CRM (ref {seed})",
            "body": ("Hi, I am Marcus Hale at Brightpath Insurance. Since Monday our setter's call "
                     "recordings have stopped syncing to our CRM and the dashboard shows an error on "
                     f"every export.\n\nCan someone take a look? I am on {phone} or {addr}."),
        }

    addr = f"casey.nguyen{seed}@northwind{seed}.example"
    return {
        "from": f"Casey Nguyen <{addr}>",
        "subject": f"Remote setters for our Q4 push ({seed})",
        "body": ("Hi, I am Casey Nguyen, revenue lead at Northwind Digital. We are launching a solar "
                 "campaign and need four appointment setters before Friday, as our current setter is "
                 f"leaving.\n\nPricing and lead times please. My email is {addr} and my direct line "
                 f"is {phone}."),
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    calls = 0

    def log_message(self, fmt, *args):
        sys.stderr.write("mock_llm %s\n" % (fmt % args))

    def _read(self) -> dict:
        n = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(n) or b"{}")

    def _send(self, payload: dict):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _content(self, text: str) -> str:
        """No-tools response: either a simulated enquiry payload or a reply draft."""
        m = SIM_CHANNEL.search(text)
        if not m:
            return DRAFT
        Handler.calls += 1
        return json.dumps(simulated_payload(m.group(1), text, Handler.calls))

    def do_POST(self):
        req = self._read()

        if self.path.rstrip("/").endswith("/messages"):  # Anthropic
            # Only the human turn — quoting the system prompt would (correctly)
            # be rejected by the grounding check.
            text = "\n".join(
                part.get("text", "")
                for m in req.get("messages", [])
                if m.get("role") in (None, "user")
                for part in (m["content"] if isinstance(m["content"], list) else [{"text": m["content"]}])
            )
            if req.get("tools"):
                self._send({
                    "id": "msg_mock", "type": "message", "role": "assistant",
                    "model": req.get("model", "mock"), "stop_reason": "tool_use",
                    "content": [{"type": "tool_use", "id": "tu_1",
                                 "name": "record_enquiry_extraction", "input": classify(text)}],
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                })
            else:
                self._send({
                    "id": "msg_mock", "type": "message", "role": "assistant",
                    "model": req.get("model", "mock"), "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": self._content(text)}],
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                })
            return

        # OpenAI chat completions
        text = "\n".join(
            str(m.get("content", "")) for m in req.get("messages", [])
            if m.get("role") == "user"
        )
        if req.get("tools"):
            msg = {"role": "assistant", "content": None, "tool_calls": [{
                "id": "call_1", "type": "function",
                "function": {"name": "record_enquiry_extraction",
                             "arguments": json.dumps(classify(text))}}]}
            finish = "tool_calls"
        else:
            msg = {"role": "assistant", "content": self._content(text)}
            finish = "stop"
        self._send({
            "id": "chatcmpl-mock", "object": "chat.completion",
            "model": req.get("model", "mock"),
            "choices": [{"index": 0, "message": msg, "finish_reason": finish}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        })


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9101
    print(f"mock LLM on http://localhost:{port}", file=sys.stderr)
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
