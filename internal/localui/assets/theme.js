/* Resolve appearance before CSS loads; the preference contains no account data. */
(() => {
  'use strict';

  const key = 'my-whatsapp-theme';
  const root = document.documentElement;
  const valid = value => value === 'light' || value === 'dark' || value === 'system';
  let preference = 'system';
  let storage;
  let media;
  let controls = [];

  try {
    storage = window.localStorage;
    const saved = storage.getItem(key);
    if (valid(saved)) preference = saved;
  } catch {
    // Private browser modes may block storage. Appearance still works in this tab.
  }
  try {
    media = window.matchMedia('(prefers-color-scheme: dark)');
  } catch {
    // Browsers without matchMedia use light until the user selects a theme.
  }

  function apply() {
    root.dataset.themePreference = preference;
    root.dataset.theme = preference === 'system' ? (media?.matches ? 'dark' : 'light') : preference;
    for (const control of controls) control.value = preference;
  }

  function choose(value) {
    if (!valid(value)) {
      apply();
      return;
    }
    preference = value;
    apply();
    try {
      storage?.setItem(key, preference);
    } catch {
      // A storage failure must not prevent a manual choice.
    }
  }

  apply();

  function connectControls() {
    controls = document.querySelectorAll('[data-theme-select]');
    for (const control of controls) control.addEventListener('change', () => choose(control.value));
    apply();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', connectControls, {once: true});
  else connectControls();

  const systemChanged = () => {if (preference === 'system') apply();};
  if (typeof media?.addEventListener === 'function') media.addEventListener('change', systemChanged);
  else if (typeof media?.addListener === 'function') media.addListener(systemChanged);

  window.addEventListener('storage', event => {
    if (event.key !== key && event.key !== null) return;
    if (storage && event.storageArea && event.storageArea !== storage) return;
    preference = event.key !== null && valid(event.newValue) ? event.newValue : 'system';
    apply();
  });
})();
