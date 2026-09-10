- The dibs cutover preflight no longer reports Let's Encrypt headroom that does
  not exist. Its quota check read a single certificate transparency source,
  crt.sh, which for `hivecommons.dev` held 46 of the 83 certificates issued in
  the rolling 168h window: a strict subset, missing 37 certificates spread
  across the whole window rather than bunched at the recent end, so the two
  services simply monitor different CT logs and a retry never cleared it. The
  check therefore reported `✓ headroom 4` and let the run proceed while the
  registered domain was already 33 over its cap of 50, which is the one gate
  that guards the irreversible, quota-spending step ([#5925](https://github.com/hivecommons/hive/issues/5925)).
  It now queries crt.sh and Cert Spotter, believes whichever sees more (neither
  can invent an issuance that did not happen, so the higher count is always the
  nearer one), and counts distinct certificates rather than CT log entries, so
  a precertificate and its leaf no longer count twice. If either source cannot
  be reached the check warns instead of passing, because a single-source answer
  is the undercount above wearing a confident number. On the same domain that
  previously passed, it now correctly reports 83 and blocks.
