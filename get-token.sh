#!/usr/bin/env bash
# Usage: ./get-token.sh [password]
#
# Gets a Keystone token scoped to your project, verifies Mimir + Grafana via
# APISIX, and prints a single browser URL that sets the cookie and lands you
# in Grafana.
#
# What you'll see in Grafana depends on your Keystone roles:
#   - If your user has the `nipa_grafana_admin` role, the APISIX Lua plugin
#     bypasses tenant scoping and gives you a cross-tenant ("admin") view.
#   - Otherwise the plugin scopes you to your project_id, and dashboards
#     only show data for that tenant.
#
# Override defaults via env vars:
#   APISIX_HOST   default: <master-ip>:31545 (master node, public NodePort)
#   OS_AUTH_URL   Keystone auth URL
#   OS_USERNAME   OpenStack username
#   OS_PROJECT_ID OpenStack project UUID

set -euo pipefail

OS_AUTH_URL="${OS_AUTH_URL:?ERROR: OS_AUTH_URL is required (Keystone auth URL)}"
OS_PROJECT_ID="${OS_PROJECT_ID:?ERROR: OS_PROJECT_ID is required (OpenStack project UUID)}"
OS_USERNAME="${OS_USERNAME:?ERROR: OS_USERNAME is required (OpenStack username)}"
OS_USER_DOMAIN_NAME="${OS_USER_DOMAIN_NAME:?ERROR: OS_USER_DOMAIN_NAME is required}"
APISIX_HOST="${APISIX_HOST:?ERROR: APISIX_HOST is required (e.g. <master-ip>:31545)}"

PASSWORD="${1:-}"
if [ -z "$PASSWORD" ]; then
  echo -n "Password for ${OS_USERNAME}: "
  read -sr PASSWORD
  echo
fi

echo "→ Requesting Keystone token..."

RESPONSE=$(curl -si -X POST "${OS_AUTH_URL}/v3/auth/tokens" \
  -H "Content-Type: application/json" \
  -d "{
    \"auth\": {
      \"identity\": {
        \"methods\": [\"password\"],
        \"password\": {
          \"user\": {
            \"name\": \"${OS_USERNAME}\",
            \"password\": \"${PASSWORD}\",
            \"domain\": {\"name\": \"${OS_USER_DOMAIN_NAME}\"}
          }
        }
      },
      \"scope\": {
        \"project\": {\"id\": \"${OS_PROJECT_ID}\"}
      }
    }
  }" 2>&1)

TOKEN=$(echo "$RESPONSE" | grep -i "^x-subject-token:" | awk '{print $2}' | tr -d '\r\n')

if [ -z "$TOKEN" ]; then
  echo "ERROR: Failed to get token. Response:"
  echo "$RESPONSE" | head -20
  exit 1
fi

echo ""
echo "✅ Token obtained for project: ${OS_PROJECT_ID}"
echo ""
echo "─────────────────────────────────────────────────"
echo "TOKEN: ${TOKEN}"
echo "─────────────────────────────────────────────────"
echo ""

# URL-encode the token for the browser link
ENCODED_TOKEN=$(python3 -c "import urllib.parse, sys; print(urllib.parse.quote(sys.argv[1]))" "${TOKEN}")

echo "─── Open in browser (sets cookie + enters Grafana automatically) ───"
echo ""
echo "  Open this URL in your browser (APISIX will set the cookie + redirect):"
echo ""
echo "     http://${APISIX_HOST}/set-token?token=${ENCODED_TOKEN}"
echo ""
echo "  (If APISIX_HOST is not directly reachable, set up an SSH tunnel first:"
echo "     ssh -L 31545:<master-ip>:31545 <user>@<master-ip>"
echo "   then re-run this script with APISIX_HOST=localhost:31545)"
echo ""

# Run the Mimir check immediately
echo "─── Running Mimir check now... ───"
curl -s -G -H "X-Auth-Token: ${TOKEN}" \
  --data-urlencode "match[]={projectId!=''}" \
  "http://${APISIX_HOST}/prometheus/api/v1/label/__name__/values" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d['data']), 'metrics for project ${OS_PROJECT_ID}')" 2>/dev/null \
  || echo "(Mimir check skipped — run from a machine that can reach ${APISIX_HOST})"

echo ""
echo "─── Running Grafana check now... ───"
GRAFANA_RESP=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 3 -b "token=${TOKEN}" "http://${APISIX_HOST}/grafana/api/user" 2>/dev/null)
if [ "$GRAFANA_RESP" = "200" ]; then
  echo "✅ Grafana accepted token — logged in as:"
  curl -s -b "token=${TOKEN}" "http://${APISIX_HOST}/grafana/api/user" \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print('  login:', d.get('login'), '| orgRole:', d.get('orgRole'))"
elif [ -z "$GRAFANA_RESP" ] || [ "$GRAFANA_RESP" = "000" ]; then
  echo "(Grafana check skipped — run from a machine that can reach ${APISIX_HOST})"
else
  echo "HTTP ${GRAFANA_RESP} — token rejected"
fi
