# Solution for Issue #6558

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
Hosted hives lack proactive agent-auth health signals. When backend authentication (e.g., Copilot license checks, token expirations) fails across agents, adopters experience silent outages discovered only by manual inspection. This creates a critical adoption bottleneck and compromises GA reliability requirements.

### Fix
Implement a lightweight per-agent auth health reporting mechanism, centralized hub telemetry row, and automated alerting trigger when all agents on a hosted hive report auth failure exceeding a configurable threshold ($N$ minutes).

### Implementation
```python
# packages/hive-core/health/auth_canary.py

import time
import logging
from typing import Dict, Literal, Optional

logger = logging.getLogger("hive.health.auth_canary")

AuthStatus = Literal["ok", "unlicensed", "token-expired", "unreachable"]

class AgentAuthProbe:
    def __init__(self, agent_id: str, hive_id: str):
        self.agent_id = agent_id
        self.hive_id = hive_id
        self.last_status: AuthStatus = "ok"
        self.last_checked_at: float = 0.0
        self.failure_streak_start: Optional[float] = None

    def record_probe(self, status: AuthStatus) -> None:
        now = time.time()
        self.last_checked_at = now
        
        if status != "ok" and self.last_status == "ok":
            self.failure_streak_start = now
        elif status == "ok":
            self.failure_streak_start = None
            
        self.last_status = status
        logger.debug(f"Agent {self.agent_id} on hive {self.hive_id} auth status: {status}")

    def is_degraded(self, threshold_seconds: float = 300.0) -> bool:
        if self.last_status == "ok" or not self.failure_streak_start:
            return False
        return (time.time() - self.failure_streak_start) >= threshold_seconds

class HiveFleetHealthMonitor:
    def __init__(self, threshold_seconds: float = 300.0):
        self.probes: Dict[str, AgentAuthProbe] = {}
        self.threshold_seconds = threshold_seconds

    def register_probe(self, probe: AgentAuthProbe) -> None:
        self.probes[probe.agent_id] = probe

    def check_hive_health(self, hive_id: str) -> Dict[str, any]:
        hive_probes = [p for p in self.probes.values() if p.hive_id == hive_id]
        if not hive_probes:
            return {"status": "unknown", "total_agents": 0, "failing_agents": 0}

        total = len(hive_probes)
        failing = sum(1 for p in hive_probes if p.is_degraded(self.threshold_seconds))

        if failing == total and total > 0:
            fleet_status = "CRITICAL_ALL_DOWN"
        elif failing > 0:
            fleet_status = "DEGRADED"
        else:
            fleet_status = "HEALTHY"

        return {
            "hive_id": hive_id,
            "status": fleet_status,
            "total_agents": total,
            "failing_agents": failing,
            "timestamp": time.time()
        }
```

### Testing
1. Unit test the `AgentAuthProbe` transition states and failure streak tracking over time.
2. Verify `HiveFleetHealthMonitor` correctly flags `CRITICAL_ALL_DOWN` when all agent probes exceed the configured `threshold_seconds` (e.g., 5 minutes).
3. Integrate health polling into the hub dashboard and webhook notification router.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`