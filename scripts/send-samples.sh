#!/usr/bin/env bash
# Posts one sample enquiry per interesting pipeline path, signed the way a real
# provider would sign it. Usage:
#   WEBHOOK_SECRET=... ./scripts/send-samples.sh [base-url]
set -euo pipefail

BASE="${1:-http://localhost:8080}"
: "${WEBHOOK_SECRET:?set WEBHOOK_SECRET (same value the API was started with)}"

post() {
  local channel="$1" body="$2" label="$3"
  local sig
  sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | sed 's/^.*= //')
  printf '\n--- %s (%s)\n' "$label" "$channel"
  curl -sS -X POST "$BASE/webhook/$channel" \
    -H 'Content-Type: application/json' \
    -H "X-Beda-Signature: sha256=$sig" \
    --data-raw "$body"
  printf '\n'
}

# High-urgency sales: routes to enterprise-sales via the priority-1 rule.
post email '{
  "from": "Jane Doe <jane.doe@acme-widgets.example>",
  "subject": "Urgent: pricing for 500 units before Friday",
  "body": "Hi,\n\nI am Jane Doe, procurement lead at Acme Widgets Ltd. We need 500 units of the WX-200 and a quote before Friday, our board signs off next week. My direct line is 020 7946 0958.\n\nThanks,\nJane"
}' "high-urgency sales"

# Support ticket from a web form: routes to support-l1.
post web_form '{
  "name": "Sam Patel",
  "email": "sam.patel@bigco.example",
  "phone": "020 7946 1234",
  "company": "BigCo Retail",
  "message": "Our checkout integration started returning 502 errors this morning and orders are not going through. Account BC-4471. This is blocking live sales."
}' "urgent support"

# Missing required info: drafts a clarifying question instead of guessing.
post messaging '{
  "sender_handle": "@curious_buyer",
  "text": "hey do you do bulk discounts?"
}' "insufficient info"

# Marketing blast: caught by heuristics, confirmed by the model, archived as junk.
post email '{
  "from": "growth@seo-deals.example",
  "subject": "Boost your ranking with premium backlinks",
  "body": "We offer premium SEO services and backlink packages. Guest post opportunities available now. https://a.example https://b.example https://c.example Click here to claim your discount."
}' "junk"

# Exact duplicate of the first enquiry: discarded before any LLM call.
post email '{
  "from": "Jane Doe <jane.doe@acme-widgets.example>",
  "subject": "Urgent: pricing for 500 units before Friday",
  "body": "Hi,\n\nI am Jane Doe, procurement lead at Acme Widgets Ltd. We need 500 units of the WX-200 and a quote before Friday, our board signs off next week. My direct line is 020 7946 0958.\n\nThanks,\nJane"
}' "duplicate of the first enquiry"

# Same person, new enquiry: matches the existing contact on email and attaches.
post email '{
  "from": "jane.doe@acme-widgets.example",
  "subject": "Follow-up: lead times",
  "body": "One more thing on the 500 unit order for Acme Widgets Ltd - what are your lead times if we sign this month? Jane Doe"
}' "returning contact"

printf '\nOpen http://localhost:3000 to review the queue.\n'
