#!/usr/bin/env bash
#
# One-time setup: create a GitHub OIDC identity provider (if absent) and a
# repo-scoped, read-only IAM role that the refresh workflow assumes to read
# Bedrock and SageMaker catalog metadata. The role has NO write permission of
# any kind and holds no long-lived credentials.
#
# Edit GITHUB_ORG / GITHUB_REPO below, then run with credentials that can
# manage IAM (iam:CreateRole, iam:CreateOpenIDConnectProvider, iam:PutRolePolicy).
#
# Idempotent: safe to re-run; it updates the trust/permission policy in place.

set -euo pipefail

# --- edit these ------------------------------------------------------------
GITHUB_ORG="scttfrdmn"
GITHUB_REPO="hf-bedrock-map"
ROLE_NAME="hf-bedrock-map-refresh"
# ---------------------------------------------------------------------------

OIDC_HOST="token.actions.githubusercontent.com"
OIDC_ARN_SUFFIX="oidc-provider/${OIDC_HOST}"

ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
OIDC_PROVIDER_ARN="arn:aws:iam::${ACCOUNT_ID}:${OIDC_ARN_SUFFIX}"
echo "Account: ${ACCOUNT_ID}"

# 1. Create the GitHub OIDC provider if it does not already exist.
if aws iam get-open-id-connect-provider --open-id-connect-provider-arn "${OIDC_PROVIDER_ARN}" >/dev/null 2>&1; then
  echo "OIDC provider already present: ${OIDC_PROVIDER_ARN}"
else
  echo "Creating OIDC provider for ${OIDC_HOST}..."
  # Thumbprint is no longer validated by AWS for this provider, but the API
  # still requires the argument; this is GitHub's long-standing value.
  aws iam create-open-id-connect-provider \
    --url "https://${OIDC_HOST}" \
    --client-id-list "sts.amazonaws.com" \
    --thumbprint-list "6938fd4d98bab03faadb97b34396831e3780aea1" >/dev/null
  echo "Created ${OIDC_PROVIDER_ARN}"
fi

# 2. Trust policy: only this repo, via OIDC, may assume the role.
#
# AWS requires the trust condition to be scoped by "sub" (or "job_workflow_ref")
# — it rejects a repository-only condition. This account's GitHub OIDC config
# emits a non-standard sub with appended numeric ids:
#   repo:${GITHUB_ORG}@NNN/${GITHUB_REPO}@NNN:ref:refs/heads/main
# so the sub pattern uses wildcards around those id suffixes. The clean
# "repository" claim is pinned as a second, exact condition for defense in
# depth, and the audience is pinned to sts.amazonaws.com.
TRUST_POLICY="$(cat <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Federated": "${OIDC_PROVIDER_ARN}" },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${OIDC_HOST}:aud": "sts.amazonaws.com",
          "${OIDC_HOST}:repository": "${GITHUB_ORG}/${GITHUB_REPO}"
        },
        "StringLike": {
          "${OIDC_HOST}:sub": "repo:${GITHUB_ORG}*/${GITHUB_REPO}*:*"
        }
      }
    }
  ]
}
JSON
)"

# 3. Permission policy: read-only Bedrock + SageMaker catalog metadata only.
PERMISSION_POLICY="$(cat <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BedrockCatalogRead",
      "Effect": "Allow",
      "Action": [
        "bedrock:ListFoundationModels",
        "bedrock:GetFoundationModel"
      ],
      "Resource": "*"
    },
    {
      "Sid": "SageMakerHubCatalogRead",
      "Effect": "Allow",
      "Action": [
        "sagemaker:ListHubContents",
        "sagemaker:DescribeHubContent"
      ],
      "Resource": "*"
    }
  ]
}
JSON
)"

if aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
  echo "Updating trust policy on existing role ${ROLE_NAME}..."
  aws iam update-assume-role-policy --role-name "${ROLE_NAME}" \
    --policy-document "${TRUST_POLICY}" >/dev/null
else
  echo "Creating role ${ROLE_NAME}..."
  aws iam create-role --role-name "${ROLE_NAME}" \
    --description "Read-only Bedrock/SageMaker catalog access for hf-bedrock-map refresh workflow" \
    --assume-role-policy-document "${TRUST_POLICY}" \
    --max-session-duration 3600 >/dev/null
fi

echo "Attaching inline read-only permission policy..."
aws iam put-role-policy --role-name "${ROLE_NAME}" \
  --policy-name "hf-bedrock-map-readonly" \
  --policy-document "${PERMISSION_POLICY}" >/dev/null

ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"
echo
echo "Done. Role ARN:"
echo "  ${ROLE_ARN}"
echo
echo "Next steps:"
echo "  1. Set repo variable HF_BEDROCK_MAP_ROLE_ARN = ${ROLE_ARN}"
echo "       gh variable set HF_BEDROCK_MAP_ROLE_ARN --body '${ROLE_ARN}'"
echo "  2. (Optional but recommended) set an HF read token as a secret so gated"
echo "     provider repos resolve during refresh:"
echo "       gh secret set HF_TOKEN --body '<hf_read_token>'"
echo "  3. Enable Pages: Settings -> Pages -> Deploy from branch -> main / /docs"
echo "  4. Trigger the workflow once (Actions -> refresh -> Run workflow)."
