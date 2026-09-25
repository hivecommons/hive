- Reject cross-origin Jam live WebSocket handshakes: the upgrader accepted any
  Origin while authenticating via the dashboard session cookie, letting a
  malicious page hijack a logged-in operator's session to read jam state and
  push live spec edits (CSWSH). Both dashboard WebSocket upgraders now share the
  same-origin CheckOrigin policy (#8809).
