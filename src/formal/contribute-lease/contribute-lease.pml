/*
 * contribute-lease.pml — protocol-level Spin model of the contributor
 * lease/reconnect ownership contract for one work item.
 *
 * Models two contributor relays, one hub, and one issue across:
 *   lease grant -> websocket drop -> disconnect release/cooldown,
 *   relay reconnect+resume, lease expiry, and reassignment.
 *
 * Time is a scaled discrete clock: one model tick is one production minute for
 * hub lease/cooldown timers. The relay's first reconnect backoff is modeled as
 * one ordered tick: BASE_RECONNECT_DELAY_MS = 1000ms. Thus the production
 * constants map as:
 *   RELEASE_COOLDOWN = 10 ticks  (failedTaskCooldownMinutes = 10)
 *   LEASE_TTL        = 30 ticks  (wsTaskTimeout / leaseTTL = 30 minutes)
 *   RECONNECT_BACKOFF = 1 tick   (BASE_RECONNECT_DELAY_MS = 1000ms)
 * The reconnect assertion is about ordering, not wall-clock magnitude.
 */

#define C0 0
#define C1 1
#define NCONTRIB 2
#define RELEASE_COOLDOWN 10
#define LEASE_TTL 30
#define RECONNECT_BACKOFF 1

#ifdef BUG_INSTANT_RELEASE
#define RELEASE_GRACE 0
#else
/* Intended #7838 reconciliation: abnormal read-fail release is delayed beyond
 * the relay's first reconnect attempt and re-checks the re-adoption guard. */
#define RELEASE_GRACE 2
#endif

#ifdef MON_RECONNECT
#define MAY_TICK (now < (LEASE_TTL + RELEASE_COOLDOWN + 2) && !(reconnectPending0 && now >= reconnectDue0 && lease_valid(C0)))
#else
#define MAY_TICK (now < (LEASE_TTL + RELEASE_COOLDOWN + 2))
#endif

byte now = 0;
bool live[NCONTRIB];          /* live hub connection currently holding the issue */
bool token[NCONTRIB];         /* contributor has a minted task credential */
bool lease[NCONTRIB];         /* hub has an unrevoked server-issued lease */
byte leaseExp[NCONTRIB];

bool cooldown = false;        /* release cooldown / failedTasks timestamp */
byte cooldownExp = 0;

bool dropped0 = false;
bool releasePending0 = false;
byte releaseDue0 = 0;

#ifdef MON_RECONNECT
bool reconnectPending0 = false;
byte reconnectDue0 = 0;
#endif

#define lease_valid(i) (lease[i] && now < leaseExp[i])

inline valid_holders(c) {
	c = 0;
	if
	:: token[C0] && lease_valid(C0) -> c++
	:: else -> skip
	fi;
	if
	:: token[C1] && lease_valid(C1) -> c++
	:: else -> skip
	fi
}

inline assert_single_holder() {
	byte holders;
	valid_holders(holders);
	assert(holders <= 1)
}

inline grant(i) {
	live[i] = true;
	token[i] = true;
	lease[i] = true;
	leaseExp[i] = now + LEASE_TTL;
	assert_single_holder()
}

inline abandon_disconnect() {
#ifdef MON_RECONNECT
	/* Required assertion #2: if the relay's first reconnect attempt is still
	 * pending inside its backoff window, the hub must not book the disconnect as
	 * abandoned. Pre-#7838 instant release violates this before the relay can
	 * make its first reconnect attempt. */
	if
	:: reconnectPending0 && now <= reconnectDue0 -> assert(false)
	:: else -> skip
	fi;
#endif
	cooldown = true;
	cooldownExp = now + RELEASE_COOLDOWN
}

inline maybe_prune_cooldown() {
	if
	:: cooldown && now >= cooldownExp -> cooldown = false
	:: else -> skip
	fi
}

inline maybe_prune_leases() {
	if
	:: lease[C0] && now >= leaseExp[C0] -> lease[C0] = false; token[C0] = false
	:: else -> skip
	fi;
	if
	:: lease[C1] && now >= leaseExp[C1] -> lease[C1] = false; token[C1] = false
	:: else -> skip
	fi
}

inline lease_hold_blocks_other(i, blocked) {
#ifdef BUG_COOLDOWN_LEASE
	/* Pre-#7773: selectTask only saw live connections and the 10-minute release
	 * cooldown; an unexpired resume lease was not an in-flight exclusion. */
	blocked = false
#else
	/* Intended #7781 semantics: an unexpired lease holds the item for every
	 * identity except its owner for exactly as long as it is resumable. */
	if
	:: i == C0 -> blocked = lease_valid(C1)
	:: i == C1 -> blocked = lease_valid(C0)
	fi
#endif
}

active proctype Protocol() {
	bool blocked;

	/* Initial task_assign: contributor 0 receives the issue, token, generation,
	 * and server-issued lease from selectTask/recordLeaseForKey. */
	grant(C0);

	do
	/* Socket drop / read-fail. The relay keeps its local currentTask and token;
	 * the hub-side live connection disappears, but the lease remains resumable. */
	:: !dropped0 ->
		live[C0] = false;
		dropped0 = true;
		releasePending0 = true;
		releaseDue0 = now + RELEASE_GRACE;
#ifdef MON_RECONNECT
		reconnectPending0 = true;
		reconnectDue0 = now + RECONNECT_BACKOFF;
#endif
		assert_single_holder()

	/* The disconnect defer runs. It books abandoned_disconnect unless another
	 * live connection for the same identity has already re-adopted the task. */
	:: releasePending0 && now >= releaseDue0 ->
		if
		:: live[C0] -> skip          /* #5337 re-adoption guard */
		:: else -> abandon_disconnect()
		fi;
		releasePending0 = false;
		assert_single_holder()

#ifdef MON_RECONNECT
	/* The relay's first reconnect attempt after BASE_RECONNECT_DELAY_MS. The
	 * auth_ok handler sends task_accepted + task_progress; the hub validates the
	 * unexpired lease, rebuilds currentTask, renews the lease, and clears any
	 * speculative release cooldown. This transition is forced before time may
	 * advance beyond reconnectDue0 to model "within the backoff window". */
	:: reconnectPending0 && now >= reconnectDue0 && lease_valid(C0) ->
		live[C0] = true;
		token[C0] = true;
		leaseExp[C0] = now + LEASE_TTL;
		reconnectPending0 = false;
		cooldown = false;
		assert_single_holder()
	:: reconnectPending0 && now >= reconnectDue0 && !lease_valid(C0) ->
		reconnectPending0 = false
#endif

	/* Contributor 1 asks for work. selectTask skips live active issues, release
	 * cooldowns, and (post-#7781) unexpired leases owned by another identity. */
	:: !live[C1] && !token[C1] ->
		maybe_prune_cooldown();
		lease_hold_blocks_other(C1, blocked);
		if
		:: !cooldown && !live[C0] && !blocked -> grant(C1)
		:: else -> skip
		fi

#ifndef MON_RECONNECT
	/* Long outage resume path: after a sleep/VPN outage, contributor 0 can still
	 * resume while its 30-minute lease is alive. In the pre-#7773 timer relation,
	 * contributor 1 can already have been assigned after the 10-minute cooldown. */
	:: dropped0 && !live[C0] && lease_valid(C0) ->
		live[C0] = true;
		token[C0] = true;
		leaseExp[C0] = now + LEASE_TTL;
		cooldown = false;
		assert_single_holder()
#endif

	/* Abstract time. In MON_RECONNECT runs, do not let the scheduler skip past the
	 * relay's due first reconnect attempt; this keeps the property about a timely
	 * reconnect rather than arbitrary process starvation. */
	:: MAY_TICK -> now++;
		maybe_prune_cooldown();
		maybe_prune_leases();
		assert_single_holder()
	:: now >= (LEASE_TTL + RELEASE_COOLDOWN + 2) -> break
	od
}
