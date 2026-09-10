### CI Enforcement & Hold Reasons

The hourly **Promote Stable Channel** workflow validates candidate stability before promotion and may report the following hold reasons:

* `candidate age <seconds> < required <seconds> (<hours>h)` — The candidate has not completed the required minimum soak duration.
* `newer candidate superseded this digest before the soak window completed` — A newer candidate was pushed and took precedence.
* `candidate <digest> comes from docker.yml run <N>, which has not completed yet; re-evaluate on the next schedule` *(Added in v4.24.4)* — Fires when the hourly promotion evaluation runs while the candidate's corresponding `docker.yml` CI run is still in progress. This is an expected, benign hold state and will automatically re-evaluate and proceed on the subsequent hourly schedule.
  * **Note for pre-v4.24.4 builds:** On versions prior to v4.24.4, this same in-flight condition manifested as a silent workflow failure (`exit 1`) with no summary output or explanation in logs. If you observe this on older versions, upgrade to v4.24.4 or later to surface explicit diagnostic hold messages.