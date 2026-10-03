// Apply the saved theme before first paint (no flash).
(function () {
  var t = null;
  try { t = localStorage.getItem('sy-theme'); } catch (e) {}
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
})();
