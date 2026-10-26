#!/usr/bin/env python3
"""CI must distinguish candidate staging from a completed CRM/Portal upgrade."""
import os
from pathlib import Path
import re
import subprocess

root=Path(__file__).resolve().parents[1]
source=(root/'bin/deploy-customer-opportunity.sh').read_text()
block=re.search(r'^if ! infrastructure_ready \|\| ! customer_runtime_ready \|\| ! portal_runtime_ready \|\| ! portal_report_worker_configured; then\n.*?^fi',source,re.M|re.S).group()
for missing in ['infrastructure_ready','customer_runtime_ready','portal_runtime_ready','portal_report_worker_configured']:
 for strict in ['true','false']:
  harness='set -Eeuo pipefail\nrelease_committed=false\n'
  for name in ['infrastructure_ready','customer_runtime_ready','portal_runtime_ready','portal_report_worker_configured']:
   harness+=name+'() { return '+('1' if name==missing else '0')+'; }\n'
  harness+='restore_release() { echo restored-release-and-runtime; }\n'
  harness+='trap \'echo "commit-state:$release_committed"\' EXIT\n'+block+'\necho real-deployment-started\n'
  p=subprocess.run(['bash','-c',harness],env=dict(os.environ,DEPLOY_FAIL_IF_RUNTIME_NOT_READY=strict),text=True,capture_output=True)
  if strict=='true':
   assert p.returncode!=0,(missing,p.stdout,p.stderr)
   assert 'restored-release-and-runtime' in p.stdout
   assert 'commit-state:false' in p.stdout
   assert '安全暂存' not in p.stdout
  else:
   assert p.returncode==0,(p.stdout,p.stderr)
   assert '安全暂存' in p.stdout
   assert 'commit-state:true' in p.stdout
  assert 'real-deployment-started' not in p.stdout
print('CRM/Portal CI readiness failure is distinct from explicit candidate staging: PASS')
