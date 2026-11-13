    // ── Shared dashboard auth headers ──
    // Bearer-token header helper used across the dashboard. It lived in
    // inception.js; the name is kept so existing callers do not change.
    function _inceptionAuthHeaders() {
      var h = {'Content-Type': 'application/json'};
      var t = localStorage.getItem('hive-token');
      if (t) h['Authorization'] = 'Bearer ' + t;
      return h;
    }
