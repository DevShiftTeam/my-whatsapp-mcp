/* Styled single selects keep the original form control as their value authority. */
(() => {
  'use strict';

  const components = new Map();
  const pending = new Set();
  let active = null;
  let scheduled = false;
  let serial = 0;

  function element(tag, className) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    return node;
  }

  function icon(path, className) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('class', className);
    const line = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    line.setAttribute('d', path);
    line.setAttribute('stroke', 'currentColor');
    line.setAttribute('stroke-width', '1.7');
    line.setAttribute('stroke-linecap', 'round');
    line.setAttribute('stroke-linejoin', 'round');
    svg.append(line);
    return svg;
  }

  function appearanceIcon(value, className) {
    const paths = {
      system: 'M4 5h16v12H4V5Zm4 15h8m-4-3v3',
      light: 'M12 3v2m0 14v2M3 12h2m14 0h2M5.6 5.6 7 7m10 10 1.4 1.4M5.6 18.4 7 17m10-10 1.4-1.4M16.5 12a4.5 4.5 0 1 1-9 0 4.5 4.5 0 0 1 9 0Z',
      dark: 'M20.4 14A8.5 8.5 0 0 1 10 3.6 8.5 8.5 0 1 0 20.4 14Z',
    };
    return icon(paths[value] || paths.system, className);
  }

  function labelText(select) {
    const explicit = select.getAttribute('aria-label');
    if (explicit) return explicit;
    const label = select.labels?.[0] || select.closest('label');
    function text(node) {
      if (node === select || node.classList?.contains('select-control')) return '';
      if (node.nodeType === 3) return node.textContent;
      return Array.from(node.childNodes || []).map(text).join(' ');
    }
    return label ? text(label).replace(/\s+/g, ' ').trim() : 'Choose an option';
  }

  function visible(component) {
    const {select, trigger} = component;
    if (!select.isConnected || select.closest('[hidden]')) return false;
    const dialog = select.closest('dialog');
    if (dialog && !dialog.open) return false;
    const details = select.closest('details');
    if (details && !details.open) return false;
    return trigger.getClientRects().length > 0;
  }

  function disabled(option) {
    return option.disabled || Boolean(option.parentElement?.tagName === 'OPTGROUP' && option.parentElement.disabled);
  }

  function hidden(option) {
    return option.hidden || Boolean(option.parentElement?.tagName === 'OPTGROUP' && option.parentElement.hidden);
  }

  function selectedOption(select) {
    return select.selectedOptions?.[0] || select.options[select.selectedIndex] || null;
  }

  function refresh(select) {
    const component = components.get(select);
    if (!component) return;
    const option = selectedOption(select);
    const shown = visible(component);
    const value = shown && option ? option.label || option.textContent : 'Choose';
    if (component.value.textContent !== value) component.value.textContent = value;
    component.trigger.setAttribute('aria-label', `${component.label}: ${value}`);
    component.trigger.disabled = select.disabled;
    component.trigger.setAttribute('aria-disabled', String(select.disabled));
    component.trigger.setAttribute('aria-required', String(select.required));
    if (component.appearance && component.iconValue !== select.value) {
      const next = appearanceIcon(select.value, 'select-appearance-icon');
      component.appearance.replaceWith(next);
      component.appearance = next;
      component.iconValue = select.value;
    }
    if (!shown || select.disabled) {
      if (active === component) close(component, false);
    } else if (active === component) {
      renderOptions(component);
      position(component);
    }
  }

  function enqueue(select) {
    pending.add(select);
    if (scheduled) return;
    scheduled = true;
    queueMicrotask(() => {
      scheduled = false;
      for (const item of pending) refresh(item);
      pending.clear();
    });
  }

  function close(component, restoreFocus = false) {
    if (!component) return;
    const focusHere = document.activeElement === component.trigger || component.panel.contains(document.activeElement);
    if (component.topLayer) {
      try { component.panel.hidePopover(); } catch { /* Already closed by the browser. */ }
    }
    component.panel.hidden = true;
    component.panel.replaceChildren();
    component.panel.removeAttribute('data-placement');
    component.trigger.setAttribute('aria-expanded', 'false');
    component.trigger.removeAttribute('aria-activedescendant');
    component.entries = [];
    component.index = -1;
    component.query = '';
    component.queryAt = 0;
    component.topLayer = false;
    if (active === component) active = null;
    if (restoreFocus && focusHere && visible(component) && !component.select.disabled) component.trigger.focus();
  }

  function closeAll() {
    close(active, false);
    for (const component of components.values()) {
      component.panel.replaceChildren();
      if (!visible(component)) refresh(component.select);
    }
  }

  function release(component) {
    close(component, false);
    component.value.textContent = 'Choose';
    component.trigger.setAttribute('aria-label', `${component.label}: Choose`);
    component.trigger.hidden = true;
    component.observer.disconnect();
    pending.delete(component.select);
    components.delete(component.select);
  }

  function inspect(component, index, scroll = true) {
    if (index < 0 || !component.entries[index] || disabled(component.entries[index].option)) return;
    if (component.index === index) {
      if (scroll) component.entries[index].node.scrollIntoView({block: 'nearest'});
      return;
    }
    const previous = component.entries[component.index];
    if (previous) previous.node.dataset.active = 'false';
    component.index = index;
    const row = component.entries[index].node;
    row.dataset.active = 'true';
    component.trigger.setAttribute('aria-activedescendant', row.id);
    if (scroll) row.scrollIntoView({block: 'nearest'});
  }

  function renderOptions(component) {
    const old = component.entries[component.index]?.option;
    const selected = selectedOption(component.select);
    const fragment = document.createDocumentFragment();
    component.entries = [];
    component.panel.replaceChildren();
    for (const option of component.select.options) {
      if (hidden(option)) continue;
      const index = component.entries.length;
      const row = element('div', 'select-option');
      row.id = `${component.panel.id}-${component.entries.length}`;
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', String(option === selected));
      row.setAttribute('aria-disabled', String(disabled(option)));
      const copy = element('span', 'select-option-copy');
      const title = element('span', 'select-option-label');
      title.textContent = option.label || option.textContent;
      title.setAttribute('dir', 'auto');
      copy.append(title);
      if (option.dataset.description) {
        const description = element('span', 'select-option-description');
        description.textContent = option.dataset.description;
        copy.append(description);
      }
      if (component.select.hasAttribute('data-theme-select')) row.append(appearanceIcon(option.value, 'select-theme-icon'));
      row.append(copy, icon('M5 12l4 4L19 6', 'select-check'));
      row.addEventListener('pointerdown', event => event.preventDefault());
      row.addEventListener('pointermove', () => {
        if (component.entries[index]?.option === option) inspect(component, index, false);
      });
      row.addEventListener('click', event => {event.preventDefault(); commit(component, option);});
      component.entries.push({option, node: row});
      fragment.append(row);
    }
    component.panel.append(fragment);
    let index = component.entries.findIndex(entry => entry.option === old && !disabled(entry.option));
    if (index < 0) index = component.entries.findIndex(entry => entry.option === selected && !disabled(entry.option));
    if (index < 0) index = component.entries.findIndex(entry => !disabled(entry.option));
    component.index = -1;
    component.trigger.removeAttribute('aria-activedescendant');
    inspect(component, index, false);
  }

  function position(component) {
    if (active !== component) return;
    const rect = component.trigger.getBoundingClientRect();
    const viewport = window.visualViewport;
    const width = viewport?.width || window.innerWidth;
    const height = viewport?.height || window.innerHeight;
    const offsetLeft = viewport?.offsetLeft || 0;
    const offsetTop = viewport?.offsetTop || 0;
    const inset = 8;
    const panelWidth = Math.min(Math.max(rect.width, component.select.hasAttribute('data-theme-select') ? 220 : 180), width - inset * 2);
    component.panel.style.width = `${panelWidth}px`;
    const below = offsetTop + height - rect.bottom - inset - 6;
    const above = rect.top - offsetTop - inset - 6;
    const desired = Math.min(component.panel.scrollHeight ? component.panel.scrollHeight + 2 : component.entries.length * 44 + 14, 320);
    const upward = below < desired && above > below;
    const maxHeight = Math.max(0, Math.min(320, upward ? above : below));
    const actualHeight = Math.min(desired, maxHeight);
    component.panel.style.maxHeight = `${maxHeight}px`;
    component.panel.style.left = `${Math.max(offsetLeft + inset, Math.min(rect.left, offsetLeft + width - panelWidth - inset))}px`;
    component.panel.style.top = `${Math.max(offsetTop + inset, upward ? rect.top - 6 - actualHeight : rect.bottom + 6)}px`;
    component.panel.dataset.placement = upward ? 'above' : 'below';
  }

  function open(component) {
    if (document.hidden || component.select.disabled || !visible(component)) return false;
    if (active === component) return true;
    close(active, false);
    active = component;
    component.panel.hidden = false;
    component.trigger.setAttribute('aria-expanded', 'true');
    renderOptions(component);
    if (typeof component.panel.showPopover === 'function') {
      component.panel.setAttribute('popover', 'manual');
      try { component.panel.showPopover(); component.topLayer = true; }
      catch { component.panel.removeAttribute('popover'); }
    }
    position(component);
    component.trigger.focus();
    inspect(component, component.index);
    return true;
  }

  function commit(component, option) {
    if (active !== component || !visible(component) || component.select.disabled || !option || disabled(option)) return;
    const index = Array.from(component.select.options).indexOf(option);
    if (index < 0 || hidden(option)) {refresh(component.select); return;}
    const changed = component.select.selectedIndex !== index;
    component.select.selectedIndex = index;
    close(component, true);
    refresh(component.select);
    if (changed) {
      component.select.dispatchEvent(new Event('input', {bubbles: true}));
      component.select.dispatchEvent(new Event('change', {bubbles: true}));
    }
  }

  function move(component, direction, end = false) {
    const entries = component.entries;
    let index = end ? (direction > 0 ? entries.length : -1) : component.index;
    const step = end ? -direction : direction;
    for (index += step; index >= 0 && index < entries.length; index += step) {
      if (!disabled(entries[index].option)) {inspect(component, index); break;}
    }
  }

  function typeahead(component, character) {
    const now = Date.now();
    component.query = now - component.queryAt > 700 ? character : component.query + character;
    component.queryAt = now;
    const query = component.query.toLocaleLowerCase();
    const repeated = Array.from(query).every(letter => letter === character.toLocaleLowerCase());
    const search = repeated ? character.toLocaleLowerCase() : query;
    const start = repeated ? component.index + 1 : component.index;
    for (let offset = 0; offset < component.entries.length; offset++) {
      const index = (Math.max(0, start) + offset) % component.entries.length;
      const option = component.entries[index].option;
      if (!disabled(option) && (option.label || option.textContent).trim().toLocaleLowerCase().startsWith(search)) {inspect(component, index); break;}
    }
  }

  function keyboard(component, event) {
    if (component.select.disabled || event.ctrlKey || event.metaKey || event.isComposing) return;
    const expanded = active === component;
    if (event.key === 'Escape' && expanded) {
      event.preventDefault(); event.stopPropagation(); close(component, true); return;
    }
    if (event.key === 'Tab') {
      if (expanded) commit(component, component.entries[component.index]?.option);
      return;
    }
    if (['ArrowDown', 'ArrowUp', 'Home', 'End', 'PageUp', 'PageDown', 'Enter', ' '].includes(event.key)) {
      event.preventDefault();
      if (!expanded) {
        if (!open(component)) return;
        if (event.key === 'ArrowDown' || event.key === 'ArrowUp') move(component, event.key === 'ArrowDown' ? 1 : -1);
        else if (event.key === 'Home' || event.key === 'End') move(component, event.key === 'End' ? 1 : -1, true);
        else if (event.key === 'PageUp' || event.key === 'PageDown') for (let i = 0; i < 10; i++) move(component, event.key === 'PageDown' ? 1 : -1);
      } else if (event.key === 'Enter' || event.key === ' ') commit(component, component.entries[component.index]?.option);
      else if (event.key === 'PageUp' || event.key === 'PageDown') for (let i = 0; i < 10; i++) move(component, event.key === 'PageDown' ? 1 : -1);
      else move(component, event.key === 'ArrowDown' || event.key === 'End' ? 1 : -1, event.key === 'Home' || event.key === 'End');
    } else if (event.key.length === 1 && !event.altKey) {
      event.preventDefault();
      if (!expanded && !open(component)) return;
      typeahead(component, event.key);
    }
  }

  function enhance(select) {
    if (select.multiple || select.size > 1 || components.has(select)) return;
    const name = select.id || `select-${++serial}`;
    const wrapper = element('div', 'select-control');
    const trigger = element('button', 'select-trigger');
    const value = element('span', 'select-value');
    const panel = element('div', 'select-panel');
    const label = labelText(select);
    const labels = Array.from(select.labels || []);
    trigger.type = 'button';
    trigger.id = `${name}-trigger`;
    trigger.dataset.selectFor = select.id || '';
    trigger.setAttribute('role', 'combobox');
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', `${name}-options`);
    value.setAttribute('dir', 'auto');
    let appearance = null;
    if (select.dataset.selectIcon === 'appearance') {
      trigger.classList.add('select-trigger-appearance');
      appearance = appearanceIcon(select.value, 'select-appearance-icon');
      trigger.append(appearance);
    }
    trigger.append(value, icon('M6 9l6 6 6-6', 'select-chevron'));
    panel.id = `${name}-options`;
    panel.setAttribute('role', 'listbox');
    panel.setAttribute('aria-label', label);
    panel.hidden = true;
    const component = {select, wrapper, trigger, value, panel, label, appearance, iconValue: select.value, entries: [], index: -1, query: '', queryAt: 0, topLayer: false, observer: null};
    components.set(select, component);
    for (const associated of labels) associated.htmlFor = trigger.id;
    select.parentNode.insertBefore(wrapper, select);
    wrapper.append(select, trigger, panel);
    select.classList.add('select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    trigger.addEventListener('click', () => {if (active === component) close(component, true); else open(component);});
    trigger.addEventListener('keydown', event => keyboard(component, event));
    select.addEventListener('change', () => refresh(select));
    select.addEventListener('invalid', event => {if (visible(component)) {event.preventDefault(); trigger.focus();}});
    select.form?.addEventListener('reset', () => enqueue(select));
    component.observer = new MutationObserver(() => enqueue(select));
    component.observer.observe(select, {childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['disabled', 'label', 'selected', 'required', 'hidden']});
    refresh(select);
  }

  for (const select of document.querySelectorAll('select')) enhance(select);

  document.addEventListener('pointerdown', event => {if (active && !active.wrapper.contains(event.target)) close(active, false);}, true);
  document.addEventListener('focusin', event => {if (active && !active.wrapper.contains(event.target)) close(active, false);});
  document.addEventListener('scroll', event => {if (active && !active.panel.contains(event.target)) close(active, false);}, true);
  window.addEventListener('resize', () => {if (active) position(active);});
  window.visualViewport?.addEventListener('resize', () => {if (active) position(active);});
  window.visualViewport?.addEventListener('scroll', () => {if (active) position(active);});
  window.addEventListener('pagehide', closeAll);
  document.addEventListener('visibilitychange', () => {if (document.hidden) closeAll();});
  document.addEventListener('close', closeAll, true);
  document.addEventListener('cancel', closeAll, true);
  new MutationObserver(records => {
    for (const component of components.values()) {
      if (!component.select.isConnected) release(component);
      else if (records.some(record => record.type === 'attributes' && record.target.contains(component.select))) refresh(component.select);
    }
  }).observe(document.body, {attributes: true, childList: true, subtree: true, attributeFilter: ['hidden', 'open']});

  window.MyWhatsAppSelects = Object.freeze({refresh, closeAll});
})();
