Stop the permissions watcher from re-logging an unfixable (EPERM) chmod of bob's `.bob` state dir at ERROR every pass: skip dirs owned by another uid, and warn once with an ownership hint.
