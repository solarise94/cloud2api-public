/* cloud2api admin — API transport layer.
 * All calls go through API.call(); when the page is opened with ?mock=1 the
 * in-memory backend (mock.js) answers instead of the network. */
(function () {
  'use strict';

  class ApiError extends Error {
    constructor(status, message, body) {
      super(message || ('HTTP ' + status));
      this.status = status;
      this.body = body || null;
    }
  }

  function mockEnabled() {
    try {
      return new URLSearchParams(location.search).get('mock') === '1';
    } catch (e) { return false; }
  }

  const API = {
    base: '/admin/api',
    isMock: mockEnabled(),
    mock: null,
    ApiError: ApiError,

    /** Perform an admin API call. Returns the envelope's `data`.
     * Throws ApiError on non-2xx. Redirects to login on 401. */
    async call(method, path, body, opts) {
      opts = opts || {};
      if (API.isMock) {
        if (!API.mock) API.mock = new window.C2AMockBackend();
        return API.mock.handle(method, path, body);
      }
      const init = { method: method, credentials: 'same-origin', headers: {} };
      if (body !== undefined) {
        init.headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(body);
      }
      let resp;
      try {
        resp = await fetch(API.base + path, init);
      } catch (e) {
        throw new ApiError(0, '网络错误：无法连接管理接口', null);
      }
      if (resp.status === 401 && !opts.noRedirect) {
        location.href = '/admin/login';
        throw new ApiError(401, '登录已过期', null);
      }
      let doc = null;
      try { doc = await resp.json(); } catch (e) { /* non-JSON */ }
      if (!resp.ok) {
        throw new ApiError(resp.status, (doc && doc.message) || ('HTTP ' + resp.status), doc);
      }
      return doc && doc.data !== undefined ? doc.data : doc;
    },

    get(path, opts) { return API.call('GET', path, undefined, opts); },
    /** GET that resolves to `null` on 404 instead of throwing — used for
     * endpoints that are not implemented yet (when later phases return 200 the
     * UI must just work). All other errors still throw.
     * 404s are remembered for 60s (sessionStorage) so navigating around or
     * reloading does not re-probe endpoints the backend does not have. */
    async getSoft(path, opts) {
      try {
        const hit = API._softMiss.get(path);
        if (hit && Date.now() - hit < 60000) return null;
      } catch (e) { /* storage unavailable — probe every time */ }
      try {
        return await API.get(path, opts);
      } catch (e) {
        if (e && e.status === 404) { API._softMiss.set(path, Date.now()); return null; }
        throw e;
      }
    },
    _softMiss: {
      get(p) { return Number(sessionStorage.getItem('c2a-soft404:' + p)) || 0; },
      set(p, t) { try { sessionStorage.setItem('c2a-soft404:' + p, String(t)); } catch (e) { /* ignore */ } },
    },
    isNotFound(e) { return !!(e && e.status === 404); },
    post(path, body) { return API.call('POST', path, body === undefined ? {} : body); },
    put(path, body) { return API.call('PUT', path, body); },
    patch(path, body) { return API.call('PATCH', path, body); },
    del(path) { return API.call('DELETE', path); },
  };

  window.C2A = window.C2A || {};
  window.C2A.API = API;
})();
