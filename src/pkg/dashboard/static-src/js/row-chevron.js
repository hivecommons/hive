    // rowChevronHtml is the shared expand/collapse button of a list row that
    // opens its details in place under the row (Customers, Meetings). It sits
    // left of the row, carries aria-expanded, and is a real button so Enter and
    // Space work. The caller draws the details region with the id it passes as
    // controlsId, and marks its own list row with the same action.
    function rowChevronHtml(action, arg, open, label, controlsId) {
      return '<button type="button" class="row-chevron" data-action="' + escapeHtml(action) + '" data-arg0="' + escapeHtml(arg) + '" aria-expanded="' + (open ? 'true' : 'false') + '"' +
        (open && controlsId ? ' aria-controls="' + escapeHtml(controlsId) + '"' : '') +
        ' aria-label="' + (open ? 'Collapse' : 'Expand') + ' ' + escapeHtml(label) + '"><span aria-hidden="true">▸</span></button>';
    }

