// Public demo only: the buttons that would change something stay visible but
// are disabled, with a tooltip. The server refuses those requests anyway
// (403), this only keeps the visitor from trying.
(function () {
  var msg = document.body.getAttribute('data-demo-msg') || 'Read-only in the public demo.';
  var sel = [
    'form[method="post"]:not([action="/logout"]):not([action="/login"]) button',
    'form[method="post"]:not([action="/logout"]):not([action="/login"]) input[type="submit"]',
    'button[hx-post]', 'button[hx-put]', 'button[hx-delete]', 'button[hx-get$="/check"]',
    'a[href$="/download"]'
  ].join(',');
  function stop(e) { e.preventDefault(); e.stopPropagation(); }
  function lock(root) {
    root.querySelectorAll(sel).forEach(function (el) {
      if (el.getAttribute('data-demo-locked')) return;
      el.setAttribute('data-demo-locked', '1');
      el.setAttribute('title', msg);
      el.setAttribute('aria-disabled', 'true');
      el.classList.add('demo-locked');
      if (el.tagName === 'A') { el.addEventListener('click', stop); } else { el.disabled = true; }
    });
  }
  lock(document);
  document.body.addEventListener('htmx:afterSwap', function () { lock(document); });
  document.body.addEventListener('htmx:load', function () { lock(document); });
})();
