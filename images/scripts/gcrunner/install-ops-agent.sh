#!/bin/bash -e
################################################################################
##  File:  install-ops-agent.sh
##  Desc:  Install the Google Cloud Ops Agent (gcrunner-specific)
##
##  The agent ships host metrics and syslog to Cloud Monitoring / Cloud Logging
##  using the attached service account. Without it, runner VMs are observability
##  blind spots — when a job goes silent (OOM, kernel panic) the VM gets deleted
##  on completion and the only diagnostic is "runner lost communication".
################################################################################

echo "=== Installing Google Cloud Ops Agent ==="

curl -fsSL https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh -o /tmp/add-ops-agent-repo.sh
bash /tmp/add-ops-agent-repo.sh --also-install
rm /tmp/add-ops-agent-repo.sh

# Make sure the agent starts on every boot. The installer enables it, but
# re-asserting here keeps the image build idempotent if the script reruns.
systemctl enable google-cloud-ops-agent

echo "=== Ops Agent installed ==="
