Fixed a `pkg/dashboard` init panic: the feedback GitHub-login validator used a Perl lookahead that Go's RE2 regexp engine rejects, which crashed the spoke on startup and every dashboard test.
