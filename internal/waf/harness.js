'use strict';
// Minimal DOM/browser shim for the Aliyun WAF challenge scripts, ported from
// docs/reverse/tools/sandbox/harness.js. The challenge engine needs only a
// handful of browser objects; implementing them explicitly keeps the solver
// free of a real DOM while it executes the unmodified vendor script.
//
// The host installs `__host*` callbacks before evaluating this file:
//   __hostNavigate(url, kind)  – location.href/replace/assign capture
//   __hostStorage(op,k,v)      – localStorage mutations (observability only)
//   __hostLog(kind, message)   – diagnostics
// `__opts.url`, `__opts.renderData` and `__opts.ua` are read at startup.
(function () {
  var opts = globalThis.__opts || {};
  var url = String(opts.url || 'https://chatglm.cn/main/alltoolsdetail');
  var renderData = String(opts.renderData || '{}');
  var ua = String(opts.ua || 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36');

  var log = globalThis.__hostLog || function () {};
  var navigate = globalThis.__hostNavigate || function () {};
  var storageHook = globalThis.__hostStorage || function () {};

  function EventTarget() {}
  EventTarget.prototype.addEventListener = function () {};
  EventTarget.prototype.removeEventListener = function () {};

  // Browsers expose window.crypto.getRandomValues. The challenge engine derives
  // part of its token payload from it, so a missing binding would make goja
  // throw mid-derivation; back it with the host CSPRNG.
  var cryptoObj = {
    getRandomValues: function (arr) {
      if (!arr || typeof arr.length !== 'number') return arr;
      var bytes = globalThis.__hostRandomBytes(Number(arr.length));
      for (var i = 0; i < arr.length; i++) arr[i] = bytes.charCodeAt(i) & 0xff;
      return arr;
    },
    randomUUID: function () {
      return globalThis.__hostUUID();
    }
  };
  Object.defineProperty(cryptoObj, 'subtle', { value: undefined });

  // Self-contained URL resolver. goja ships no WHATWG URL object, and relying
  // on one silently truncates the pathname — the challenge builds its
  // navigation target from location.pathname, so that would emit a token for
  // the wrong page (and the WAF would reject it).
  function splitOrigin(s) {
    var m = /^([a-zA-Z][a-zA-Z0-9+.\-]*:)\/\/([^\/?#]*)([\s\S]*)$/.exec(s);
    if (!m) return null;
    var protocol = m[1].toLowerCase(), host = m[2].toLowerCase(), rest = m[3] || '';
    var hostname = host, port = '';
    var hp = /^(\[[^\]]*\]|[^:]*):(\d*)$/.exec(host);
    if (hp) { hostname = hp[1]; port = hp[2]; }
    return { protocol: protocol, host: host, hostname: hostname, port: port, rest: rest };
  }

  function splitRest(rest) {
    var hash = '', search = '';
    var h = rest.indexOf('#');
    if (h >= 0) { hash = rest.slice(h); rest = rest.slice(0, h); }
    var q = rest.indexOf('?');
    if (q >= 0) { search = rest.slice(q); rest = rest.slice(0, q); }
    return { pathname: rest || '/', search: search, hash: hash };
  }

  function parseUrl(value) {
    var s = String(value == null ? '' : value).trim();
    var originPart = splitOrigin(s);
    if (!originPart) {
      var base = splitOrigin(url);
      if (!base) {
        return { protocol: 'https:', host: 'chatglm.cn', hostname: 'chatglm.cn', port: '',
                 pathname: '/', search: '', hash: '', origin: 'https://chatglm.cn', href: s };
      }
      var baseRest = splitRest(base.rest);
      var abs;
      if (s.indexOf('//') === 0) {
        abs = base.protocol + s;
      } else if (s.charAt(0) === '/') {
        abs = base.protocol + '//' + base.host + s;
      } else if (s.charAt(0) === '?') {
        abs = base.protocol + '//' + base.host + baseRest.pathname + s;
      } else if (s.charAt(0) === '#') {
        abs = base.protocol + '//' + base.host + baseRest.pathname + baseRest.search + s;
      } else if (s === '') {
        abs = base.protocol + '//' + base.host + baseRest.pathname + baseRest.search + baseRest.hash;
      } else {
        var dir = baseRest.pathname.slice(0, baseRest.pathname.lastIndexOf('/') + 1);
        abs = base.protocol + '//' + base.host + dir + s;
      }
      originPart = splitOrigin(abs);
      if (!originPart) {
        return { protocol: 'https:', host: 'chatglm.cn', hostname: 'chatglm.cn', port: '',
                 pathname: '/', search: '', hash: '', origin: 'https://chatglm.cn', href: abs };
      }
    }
    var parts = splitRest(originPart.rest);
    var origin = originPart.protocol + '//' + originPart.host;
    return {
      protocol: originPart.protocol, host: originPart.host, hostname: originPart.hostname,
      port: originPart.port, pathname: parts.pathname, search: parts.search, hash: parts.hash,
      origin: origin, href: origin + parts.pathname + parts.search + parts.hash
    };
  }

  function makeElement(tag) {
    var el = {
      tagName: String(tag).toUpperCase(), nodeType: 1, style: {}, childNodes: [], attributes: {},
      appendChild: function () {}, insertBefore: function () {}, removeChild: function () {},
      setAttribute: function (k, v) { el.attributes[k] = String(v); },
      getAttribute: function (k) { return Object.prototype.hasOwnProperty.call(el.attributes, k) ? el.attributes[k] : null; },
      removeAttribute: function (k) { delete el.attributes[k]; },
      addEventListener: function () {}, removeEventListener: function () {},
      getContext: function () { return null; },
      querySelector: function () { return null; }, querySelectorAll: function () { return []; },
      getElementsByTagName: function () { return []; },
      cloneNode: function () { return makeElement(tag); },
      textContent: '', innerText: '', className: '', id: '',
      offsetWidth: 0, offsetHeight: 0, clientWidth: 0, clientHeight: 0
    };

    // Any element whose innerHTML is assigned exposes a firstChild <a> whose
    // href property parses the URL — the WAF's URL parser of choice.
    var firstChild = null;
    function makeAnchor() {
      var a = {
        tagName: 'A', nodeType: 1, style: {}, childNodes: [], attributes: {},
        appendChild: function () {}, setAttribute: function (k, v) { a.attributes[k] = String(v); },
        getAttribute: function (k) { return Object.prototype.hasOwnProperty.call(a.attributes, k) ? a.attributes[k] : null; },
        removeAttribute: function () {}, addEventListener: function () {}, removeEventListener: function () {},
        innerHTML: '', textContent: ''
      };
      var href = '';
      Object.defineProperty(a, 'href', {
        get: function () { return href; },
        set: function (v) {
          href = String(v);
          var u = parseUrl(href);
          a.protocol = u.protocol; a.host = u.host; a.hostname = u.hostname; a.port = u.port;
          a.pathname = u.pathname; a.search = u.search; a.hash = u.hash; a.origin = u.origin;
        },
        configurable: true
      });
      return a;
    }
    var innerHTML = '';
    Object.defineProperty(el, 'innerHTML', {
      get: function () { return innerHTML; },
      set: function (v) { innerHTML = String(v); firstChild = makeAnchor(); },
      configurable: true
    });
    Object.defineProperty(el, 'firstChild', {
      get: function () { return firstChild; },
      set: function (v) { firstChild = v; },
      configurable: true
    });
    return el;
  }

  var storage = {};
  var localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(storage, k) ? storage[k] : null; },
    setItem: function (k, v) { storage[k] = String(v); storageHook('set', String(k), String(v)); },
    removeItem: function (k) { delete storage[k]; storageHook('remove', String(k), ''); },
    clear: function () { storage = {}; storageHook('clear', '', ''); },
    key: function (i) { return Object.keys(storage)[i] || null; }
  };
  Object.defineProperty(localStorage, 'length', { get: function () { return Object.keys(storage).length; } });

  var cookieJar = {};
  var document = {
    nodeType: 9, readyState: 'complete',
    documentElement: makeElement('html'),
    head: makeElement('head'),
    body: makeElement('body'),
    createElement: function (tag) { return makeElement(tag); },
    createDocumentFragment: function () { return makeElement('#fragment'); },
    createTextNode: function (t) { return { nodeType: 3, textContent: String(t) }; },
    getElementById: function (id) {
      if (id === 'renderData') return { innerHTML: renderData, textContent: renderData, style: {}, getAttribute: function () { return null; } };
      return null;
    },
    getElementsByTagName: function (tag) {
      if (String(tag).toLowerCase() === 'script') {
        var src = String(opts.scriptSrc || 'https://chatglm.cn/u21pn7x6/r8lw5pzu/psk8uqfi');
        return [{ src: src, getAttribute: function (k) { return k === 'src' ? this.src : null; } }];
      }
      return [];
    },
    getElementsByName: function () { return []; },
    currentScript: { src: String(opts.scriptSrc || 'https://chatglm.cn/u21pn7x6/r8lw5pzu/psk8uqfi'),
                     getAttribute: function (k) { return k === 'src' ? this.src : null; } },
    referrer: '',
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    addEventListener: function () {}, removeEventListener: function () {},
    attachEvent: function () {}, detachEvent: function () {}
  };
  Object.defineProperty(document, 'cookie', {
    get: function () {
      return Object.keys(cookieJar).map(function (k) { return k + '=' + cookieJar[k]; }).join('; ');
    },
    set: function (v) {
      var pair = String(v).split(';')[0];
      var idx = pair.indexOf('=');
      if (idx > 0) {
        var k = pair.slice(0, idx).trim();
        cookieJar[k] = pair.slice(idx + 1).trim();
        storageHook('cookie', k, cookieJar[k]);
      }
    },
    configurable: true
  });

  var location = {
    href: url,
    replace: function (u) { navigate(String(u), 'replace'); },
    assign: function (u) { navigate(String(u), 'assign'); },
    reload: function () { navigate(url, 'reload'); }
  };
  var parsed = parseUrl(url);
  ['protocol', 'host', 'hostname', 'port', 'pathname', 'search', 'hash', 'origin'].forEach(function (k) {
    Object.defineProperty(location, k, { get: function () { return parsed[k]; }, set: function (v) { parsed[k] = String(v); }, configurable: true });
  });
  Object.defineProperty(location, 'href', {
    get: function () { return parsed.href; },
    set: function (v) { parsed = parseUrl(v); navigate(parsed.href, 'href'); },
    configurable: true
  });

  var navigator = {
    userAgent: ua, appName: 'Netscape', appVersion: ua, platform: 'Win32',
    language: 'zh-CN', languages: ['zh-CN', 'zh'], cookieEnabled: true,
    hardwareConcurrency: 8, deviceMemory: 8, webdriver: false,
    plugins: [], mimeTypes: [], vendor: 'Google Inc.'
  };

  var win = globalThis;
  win.window = win;
  win.self = win;
  win.top = win;
  win.parent = win;
  win.globalThis = win;
  win.document = document;
  win.location = location;
  win.navigator = navigator;
  win.crypto = cryptoObj;
  win.localStorage = localStorage;
  win.sessionStorage = localStorage;
  win.screen = { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040, colorDepth: 24, availLeft: 0, availTop: 0 };
  win.innerWidth = 1920; win.innerHeight = 947; win.outerWidth = 1920; win.outerHeight = 1040;
  win.devicePixelRatio = 1;
  win.performance = { now: function () { return Date.now(); }, timing: {}, navigation: {} };
  win.doScroll = function () {};
  win.attachEvent = function () {};
  win.detachEvent = function () {};
  win.execScript = function () {};
  win.console = { log: function () {}, error: function () {}, warn: function () {}, info: function () {}, debug: function () {} };
  win.atob = function (s) { return globalThis.__atob(String(s)); };
  win.btoa = function (s) { return globalThis.__btoa(String(s)); };
  win.addEventListener = function () {};
  win.removeEventListener = function () {};

  function XHR() {
    this.readyState = 0; this.status = 0; this.responseText = ''; this.response = '';
    this.responseType = ''; this.withCredentials = false; this.headers = {};
  }
  XHR.prototype.open = function (m, u) { this.method = m; this.url = u; };
  XHR.prototype.setRequestHeader = function (k, v) { this.headers[k] = v; };
  XHR.prototype.getAllResponseHeaders = function () { return ''; };
  XHR.prototype.abort = function () {};
  XHR.prototype.addEventListener = function (t, f) { if (t === 'load') this.onload = f; if (t === 'readystatechange') this.onreadystatechange = f; };
  XHR.prototype.send = function () {
    var self = this;
    log('xhr', String(this.method) + ' ' + String(this.url));
    setTimeout(function () {
      self.readyState = 4; self.status = 200; self.responseText = '{}'; self.response = '{}';
      try { if (self.onreadystatechange) self.onreadystatechange(); } catch (e) {}
      try { if (self.onload) self.onload(); } catch (e) {}
    }, 5);
  };
  win.XMLHttpRequest = XHR;
  win.fetch = function (u) {
    log('fetch', String(u));
    return Promise.resolve({ ok: true, status: 200, text: function () { return Promise.resolve('{}'); }, json: function () { return Promise.resolve({}); }, headers: { get: function () { return null; } } });
  };
  win.open = function (u) { log('window.open', String(u)); };
})();
