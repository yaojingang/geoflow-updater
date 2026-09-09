#!/usr/bin/python3
"""Root-owned fault injection around the real Docker CLI on disposable test hosts."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

STATE = Path('/var/lib/geoflow-updater')
FAULT = STATE / 'planned-rehearsal-fault.json'
MARKER = STATE / 'planned-rehearsal-hit.json'


def matches(fault, transaction, arguments):
    if fault['stage'] == 'fresh-install':
        return 'geoflow:install' in arguments and 'artisan' in arguments
    return transaction.get('status') == 'running' and transaction.get('stage') == fault['stage']


def main():
    arguments = sys.argv[1:]
    if (STATE / 'planned-rehearsal-block-restore').exists() and 'pg_restore' in arguments:
        return 98
    fault = json.loads(FAULT.read_text()) if FAULT.exists() else None
    transaction_path = STATE / 'instances/primary/release-transaction.json'
    transaction = json.loads(transaction_path.read_text()) if transaction_path.exists() else {}
    if fault and matches(fault, transaction, arguments) and not MARKER.exists():
        if fault.get('after'):
            result = subprocess.run(['/usr/bin/docker', *arguments])
            if result.returncode:
                return result.returncode
        with MARKER.open('x') as marker:
            json.dump({'stage': fault['stage'], 'after': fault.get('after', False)}, marker)
        while FAULT.exists():
            time.sleep(1)
        if fault.get('after'):
            return 0
    os.execv('/usr/bin/docker', ['/usr/bin/docker', *arguments])


if __name__ == '__main__':
    sys.exit(main())
