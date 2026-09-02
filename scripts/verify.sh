#!/usr/bin/env bash
# End-to-end check of the pipeline against a running API. Asserts the behaviours
# the brief cares about: webhook auth, idempotency, dedupe, routing, the approval
# gate, role scoping, and the audit trail. Exits non-zero on the first failure.
#
#   BASE=http://127.0.0.1:8099 WEBHOOK_SECRET=... ./scripts/verify.sh
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8080}"
: "${WEBHOOK_SECRET:?set WEBHOOK_SECRET (same value the API was started with)}"

pass=0 fail=0

ok() { printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
no() { printf '  FAIL  %s\n     %s\n' "$1" "${2:-}"; fail=$((fail+1)); }

check() { # check <label> <expected> <actual>
  if [[ "$2" == "$3" ]]; then ok "$1"; else no "$1" "expected '$2', got '$3'"; fi
}

sign() { printf '%s' "$1" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | sed 's/^.*= //'; }

post() { # post <channel> <body> -> response body
  curl -sS -X POST "$BASE/webhook/$1" -H 'Content-Type: application/json' \
    -H "X-Beda-Signature: sha256=$(sign "$2")" --data-raw "$2"
}

code() { # code <method> <path> [actor] [body] -> status code
  local m=$1 p=$2 a=${3:-} b=${4:-}
  curl -sS -o /dev/null -w '%{http_code}' -X "$m" "$BASE$p" \
    ${a:+-H "X-Actor-Id: $a"} -H 'Content-Type: application/json' ${b:+--data-raw "$b"}
}

jq_py() { python -c "import sys,json;$1" ; }

# Every enquiry carries a per-run tag. Without it the second run of this script
# would be deduplicated against the first and assert against an already-approved
# message. The tag goes in the sender address so each run also gets its own contact.
RUN="${RUN:-$(date +%s)}"

echo "== webhook authentication"
BODY='{"from":"auth@probe.example","body":"This is a probe enquiry with enough words to pass."}'
check "unsigned request is rejected"    401 "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/webhook/email" --data-raw "$BODY")"
check "bad signature is rejected"       401 "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/webhook/email" -H 'X-Beda-Signature: sha256=00' --data-raw "$BODY")"
check "unknown channel is rejected"     400 "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/webhook/pigeon" -H "Authorization: Bearer $WEBHOOK_SECRET" --data-raw "$BODY")"
check "empty body is rejected"          400 "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/webhook/email" -H "Authorization: Bearer $WEBHOOK_SECRET" --data-raw '{"from":"a@b.example","body":"  "}')"

echo "== ingestion and idempotency"
SALES_BODY='{"from":"Jane Doe <jane.doe+'"$RUN"'@verify.example>","subject":"Pricing for 500 units before Friday","body":"Hi, I am Jane Doe, procurement lead at Verify Widgets Ltd. We need a quote for 500 units of the WX-200 before Friday. My direct line is 020 7946 0100 and my email is jane.doe+'"$RUN"'@verify.example."}'
FIRST=$(post email "$SALES_BODY")
EID=$(printf '%s' "$FIRST" | jq_py "print(json.load(sys.stdin)['enquiry_id'])")
[[ -n "$EID" ]] && ok "enquiry accepted ($EID)" || no "enquiry accepted" "$FIRST"

AGAIN=$(post email "$SALES_BODY")
check "redelivery returns the same id"  "$EID" "$(printf '%s' "$AGAIN" | jq_py "print(json.load(sys.stdin)['enquiry_id'])")"
check "redelivery reports already_received" already_received "$(printf '%s' "$AGAIN" | jq_py "print(json.load(sys.stdin)['status'])")"

echo "== actors and role scoping"
USERS=$(curl -sS "$BASE/api/users")
role_id() { printf '%s' "$USERS" | jq_py "print(next(u['id'] for u in json.load(sys.stdin) if u['role']=='$1'))"; }
OPS=$(role_id ops_admin); MGR=$(role_id manager); SUP=$(role_id support_agent)
SALES_ENT=$(printf '%s' "$USERS" | jq_py "print(next(u['id'] for u in json.load(sys.stdin) if u['team']=='enterprise-sales'))")
check "queue requires an actor"         401 "$(code GET /api/queue)"
check "unknown actor is rejected"       401 "$(code GET /api/queue 00000000-0000-0000-0000-000000000000)"

echo "== pipeline processing (waiting for the worker)"
for _ in $(seq 40); do
  STATUS=$(curl -sS -H "X-Actor-Id: $OPS" "$BASE/api/enquiries/$EID" | jq_py "print(json.load(sys.stdin)['enquiry']['status'])")
  [[ "$STATUS" == "pending_approval" || "$STATUS" == "failed" || "$STATUS" == "needs_human_review" ]] && break
  sleep 1
done
check "enquiry reached pending_approval" pending_approval "$STATUS"

DETAIL=$(curl -sS -H "X-Actor-Id: $OPS" "$BASE/api/enquiries/$EID")
get() { printf '%s' "$DETAIL" | jq_py "d=json.load(sys.stdin);print($1)"; }
check "classified as sales"             sales "$(get "d['extraction']['enquiry_type']")"
check "urgency is high"                 high  "$(get "d['extraction']['urgency']")"
check "routed to enterprise-sales"      enterprise-sales "$(get "d['crm_record']['team']")"
check "an owner was assigned"           True  "$(get "d['owner_name'] is not None")"
check "no ungrounded fields"            "[]"  "$(get "d['extraction']['unverified_fields'] or []")"
check "a reply was drafted"             reply "$(get "d['messages'][0]['kind']")"
check "the draft awaits approval"       pending_approval "$(get "d['messages'][0]['status']")"
check "nothing has been sent"           True  "$(get "all(m.get('sent_at') is None for m in d['messages'])")"
for action in received classified routed crm_record_upserted reply_drafted; do
  check "audit records '$action'"        True  "$(get "'$action' in [a['action'] for a in d['audit']]")"
done

echo "== duplicate detection"
DUP=$(post email "$SALES_BODY")
check "exact duplicate is not reprocessed" already_received "$(printf '%s' "$DUP" | jq_py "print(json.load(sys.stdin)['status'])")"

echo "== the approval gate"
MID=$(get "d['messages'][0]['id']")
check "manager cannot approve"          403 "$(code POST "/api/messages/$MID/decision" "$MGR" '{"decision":"approved"}')"
check "another team cannot approve"     403 "$(code POST "/api/messages/$MID/decision" "$SUP" '{"decision":"approved"}')"
check "invalid decision is rejected"    400 "$(code POST "/api/messages/$MID/decision" "$SALES_ENT" '{"decision":"send_it"}')"
check "edit with no body is rejected"   400 "$(code POST "/api/messages/$MID/decision" "$SALES_ENT" '{"decision":"approved_with_edit","body":"   "}')"
check "the owning team can approve"     200 "$(code POST "/api/messages/$MID/decision" "$SALES_ENT" '{"decision":"approved_with_edit","body":"Hi Jane, quote attached. Lead time is three weeks.","notes":"tightened wording"}')"
check "a second approval is refused"    409 "$(code POST "/api/messages/$MID/decision" "$SALES_ENT" '{"decision":"approved"}')"

AFTER=$(curl -sS -H "X-Actor-Id: $OPS" "$BASE/api/enquiries/$EID")
aget() { printf '%s' "$AFTER" | jq_py "d=json.load(sys.stdin);print($1)"; }
check "message is now sent"             sent "$(aget "d['messages'][0]['status']")"
check "the edit was stored"             True "$(aget "'three weeks' in d['messages'][0]['body']")"
check "the approver is on the record"   True "$(aget "d['messages'][0]['drafted_by'].startswith('user:')")"
check "the human action is audited"     True "$(aget "any(a['action']=='message_sent' and a['actor'].startswith('user:') for a in d['audit'])")"

echo "== junk and insufficient-info paths"
JUNK=$(post email '{"from":"growth+'"$RUN"'@spam.example","subject":"Boost your ranking with premium backlinks","body":"We offer premium SEO services and backlink packages. Guest post opportunities. https://a.example https://b.example https://c.example Click here to claim your discount. ref '"$RUN"'"}')
JID=$(printf '%s' "$JUNK" | jq_py "print(json.load(sys.stdin)['enquiry_id'])")
THIN=$(post messaging '{"sender_handle":"@probe_user_'"$RUN"'","text":"hey do you do bulk discounts?"}')
TID=$(printf '%s' "$THIN" | jq_py "print(json.load(sys.stdin)['enquiry_id'])")
sleep 12
JSTATUS=$(curl -sS -H "X-Actor-Id: $OPS" "$BASE/api/enquiries/$JID" | jq_py "print(json.load(sys.stdin)['enquiry']['status'])")
check "junk is archived, no CRM record" archived_junk "$JSTATUS"
TDETAIL=$(curl -sS -H "X-Actor-Id: $OPS" "$BASE/api/enquiries/$TID")
check "thin enquiry asks a question"    clarifying_question "$(printf '%s' "$TDETAIL" | jq_py "print(json.load(sys.stdin)['messages'][0]['kind'])")"
check "the question awaits approval"    pending_approval    "$(printf '%s' "$TDETAIL" | jq_py "print(json.load(sys.stdin)['messages'][0]['status']) ")"
check "no CRM record was invented"      True "$(printf '%s' "$TDETAIL" | jq_py "print(json.load(sys.stdin)['crm_record'] is None)")"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
