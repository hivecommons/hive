/**
 * @name Untrusted GitHub text reaches an agent kick without ioscan
 * @description Issue and pull-request titles and bodies, and comment bodies,
 *              are written by anyone. Text that reaches the kick message an
 *              agent works from must first pass ioscan.EnforceInputFrom,
 *              directly or through a scheduler wrapper (enforceTextFrom,
 *              enforceThread, enforceLabelsWithPolicy); otherwise a prompt
 *              injection lands in the agent's instructions unscanned.
 * @kind path-problem
 * @problem.severity error
 * @security-severity 8.1
 * @precision medium
 * @id hive/go/untrusted-github-text-to-kick
 * @tags security
 *       external/cwe/cwe-077
 */

import go

/**
 * Holds if `pkg` is the hive package `name`. Matched by suffix so the query
 * applies both to github.com/hivecommons/hive and to the test fixture module.
 */
bindingset[pkg, name]
predicate isHivePackage(string pkg, string name) { pkg.matches("%/pkg/" + name) }

/** A read of a free-text field of a GitHub issue, pull request or comment. */
class UntrustedGitHubText extends DataFlow::FieldReadNode {
  UntrustedGitHubText() {
    exists(string pkg, string tp, string fld |
      this.getField().hasQualifiedName(pkg, tp, fld) and
      isHivePackage(pkg, "github")
    |
      tp = ["Issue", "PullRequest"] and fld = ["Title", "Body"]
      or
      tp = ["PRComment", "ThreadComment", "ReviewInlineComment"] and fld = "Body"
    )
  }
}

/** A call that scans untrusted text with ioscan before it is used. */
class IoscanEnforcement extends DataFlow::CallNode {
  IoscanEnforcement() {
    exists(Function f, string pkg |
      f.hasQualifiedName(pkg,
        ["EnforceInputFrom", "EnforceInput", "EnforceThreadFrom", "EnforceThread"]) and
      isHivePackage(pkg, "ioscan") and
      this = f.getACall()
    )
    or
    exists(Method m, string pkg |
      m.hasQualifiedName(pkg, "Scheduler",
        ["enforceTextFrom", "enforceIssueTextVerdict", "enforceThread", "enforceLabelsWithPolicy"]) and
      isHivePackage(pkg, "scheduler") and
      this = m.getACall()
    )
  }
}

/** The value stored as a scheduler KickMessage's Message. */
predicate isKickMessage(DataFlow::Node sink) {
  exists(StructLit lit, KeyValueExpr kv, string pkg |
    lit.getType().hasQualifiedName(pkg, "KickMessage") and
    isHivePackage(pkg, "scheduler") and
    kv = lit.getAnElement() and
    kv.getKey().(Ident).getName() = "Message" and
    sink.asExpr() = kv.getValue()
  )
  or
  exists(Write w, Field f, string pkg |
    f.hasQualifiedName(pkg, "KickMessage", "Message") and
    isHivePackage(pkg, "scheduler") and
    w.writesField(_, f, sink)
  )
}

module UntrustedTextToKickConfig implements DataFlow::ConfigSig {
  predicate isSource(DataFlow::Node source) { source instanceof UntrustedGitHubText }

  predicate isSink(DataFlow::Node sink) { isKickMessage(sink) }

  // Text handed to ioscan is the sanctioned path: what comes back is the
  // scanned (and, when the policy trips, redacted) text.
  predicate isBarrier(DataFlow::Node node) {
    node = any(IoscanEnforcement call).getAnArgument()
  }
}

module UntrustedTextToKickFlow = TaintTracking::Global<UntrustedTextToKickConfig>;

import UntrustedTextToKickFlow::PathGraph

from UntrustedTextToKickFlow::PathNode source, UntrustedTextToKickFlow::PathNode sink
where UntrustedTextToKickFlow::flowPath(source, sink)
select sink.getNode(), source, sink,
  "GitHub-authored $@ reaches this kick message without passing ioscan.EnforceInputFrom.",
  source.getNode(), source.getNode().(UntrustedGitHubText).getField().getName()
