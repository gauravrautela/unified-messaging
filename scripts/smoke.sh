#!/usr/bin/env bash
# Smoke-test the core mail API against a running server and write every
# request/response pair to a report file.
#
#   set -a && source .env && set +a
#   scripts/smoke.sh [account_id] [report_path]
#
# Sends one email from the account to itself. Requires: curl, python3, API_KEY.
set -u

BASE="${BASE_URL:-http://localhost:8080}"
ACC="${1:-}"
OUT="${2:-docs/smoke-report.md}"
: "${API_KEY:?API_KEY is required}"
AUTH="Authorization: Bearer $API_KEY"
PASS=0; FAIL=0

if [ -z "$ACC" ]; then
  ACC=$(curl -s -H "$AUTH" "$BASE/api/v1/accounts" | python3 -c 'import sys,json; i=json.load(sys.stdin)["items"]; print(i[0]["id"] if i else "")')
fi
[ -n "$ACC" ] || { echo "no connected account found"; exit 1; }

{
  echo "# API smoke test"
  echo
  echo "- Run at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "- Server: $BASE"
  echo "- Account: \`$ACC\`"
  echo
} > "$OUT"

# step <title> <expected_status> <curl args...>
# Records the redacted command, status, headers of interest and body.
step() {
  local title="$1" want="$2"; shift 2
  local hdr body code
  hdr=$(mktemp); body=$(mktemp)
  if [ "${NOAUTH:-}" = 1 ]; then
    curl -sS -D "$hdr" -o "$body" -w '%{http_code}' "$@" > "$hdr.code"
  else
    curl -sS -D "$hdr" -o "$body" -w '%{http_code}' -H "$AUTH" "$@" > "$hdr.code"
  fi
  code=$(cat "$hdr.code")
  local ok="PASS"; [ "$code" = "$want" ] || ok="FAIL"
  if [ "$ok" = PASS ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi
  echo "[$ok] $title -> $code (want $want)"
  {
    echo "## $title"
    echo
    echo '```bash'
    if [ "${NOAUTH:-}" = 1 ]; then printf 'curl'; else printf 'curl -H "Authorization: Bearer $API_KEY"'; fi
    python3 -c 'import sys,shlex; print(" " + " ".join(shlex.quote(a) for a in sys.argv[1:]))' "$@"
    echo '```'
    echo
    echo "**Response:** HTTP $code — $ok (expected $want)"
    echo
    grep -i '^content-type\|^content-disposition\|^content-length' "$hdr" | sed 's/^/    /'
    echo
    if grep -qi '^content-type: application/json' "$hdr"; then
      echo '```json'
      python3 -c 'import sys,json
d=json.load(open(sys.argv[1]))
def trim(o):
    if isinstance(o,dict): return {k:(trim(v) if k!="body" else f"<{len(v)} chars>") for k,v in o.items()}
    if isinstance(o,list): return [trim(x) for x in o]
    return o
print(json.dumps(trim(d),indent=2))' "$body"
      echo '```'
    else
      echo "    (binary, $(wc -c < "$body" | tr -d ' ') bytes: $(file -b "$body"))"
    fi
    echo
  } >> "$OUT"
  cp "$body" "$hdr.body"
  LAST_BODY="$hdr.body"
}

step "GET /api/v1/accounts/{id}" 200 "$BASE/api/v1/accounts/$ACC"
step "GET /api/v1/accounts/{id} — unknown id" 404 "$BASE/api/v1/accounts/acc_does_not_exist"

step "GET /api/v1/emails — inbox, newest 3" 200 "$BASE/api/v1/emails?account_id=$ACC&folder_role=inbox&limit=3"
# Prefer a message with an attachment so the download step is exercised.
MSG=$(curl -s -H "$AUTH" "$BASE/api/v1/emails?account_id=$ACC&folder_role=inbox&limit=50" | python3 -c '
import sys,json
items=json.load(sys.stdin)["items"]
withatt=[e for e in items if e.get("has_attachments")]
print((withatt or items or [{"id":""}])[0]["id"])')
step "GET /api/v1/emails — missing account_id" 400 "$BASE/api/v1/emails"
NOAUTH=1 step "GET /api/v1/emails — no API key" 401 "$BASE/api/v1/emails?account_id=$ACC"

if [ -n "$MSG" ]; then
  step "GET /api/v1/emails/{id}" 200 "$BASE/api/v1/emails/$MSG?account_id=$ACC"
  step "GET /api/v1/emails/{id}/attachments" 200 "$BASE/api/v1/emails/$MSG/attachments?account_id=$ACC"
  ATT=$(python3 -c 'import sys,json; i=json.load(open(sys.argv[1]))["items"]; print(i[0]["id"] if i else "")' "$LAST_BODY")
  if [ -n "$ATT" ]; then
    step "GET /api/v1/emails/{id}/attachments/{aid}" 200 "$BASE/api/v1/emails/$MSG/attachments/$ATT?account_id=$ACC"
  else
    echo "(newest inbox message has no attachment; download step skipped)" | tee -a "$OUT"
  fi
fi

SELF=$(curl -s -H "$AUTH" "$BASE/api/v1/accounts/$ACC" | python3 -c 'import sys,json; print(json.load(sys.stdin)["email"])')
SUBJ="smoke-test $(date -u +%H:%M:%S)"
step "POST /api/v1/emails — send to self" 202 -X POST -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$ACC\",\"to\":[{\"email\":\"$SELF\"}],\"subject\":\"$SUBJ\",\"body\":\"<p>Sent by scripts/smoke.sh</p>\"}" \
  "$BASE/api/v1/emails"
step "POST /api/v1/emails — missing recipients" 400 -X POST -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$ACC\",\"subject\":\"x\",\"body\":\"y\"}" "$BASE/api/v1/emails"

{
  echo "## Summary"
  echo
  echo "- Passed: $PASS"
  echo "- Failed: $FAIL"
  echo "- Sent one email to \`$SELF\` with subject \`$SUBJ\`"
} >> "$OUT"
echo "passed=$PASS failed=$FAIL report=$OUT"
[ "$FAIL" -eq 0 ]
