(function () {
  'use strict';
  var params = new URLSearchParams(location.search);
  var id = params.get('storage_id');
  if (!params.toString()) {
    location.replace('/admin/#/drives');
    return;
  }
  if (!/^[1-9][0-9]{0,18}$/.test(id || '')) return;
  // A document on the portal origin makes the next navigation same-site,
  // retaining the Strict session cookie even when OpenList uses another host.
  location.replace('/admin/#/drives?connected=' + encodeURIComponent(id));
})();
