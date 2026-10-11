import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';

// Exercise the shipped component through DOM events and its production refresh API.
// Names, options and browser state are synthetic; no account or HTTP request is used.
const source = readFileSync(new URL('../../internal/localui/assets/selects.js', import.meta.url), 'utf8');
const cases = [], failures = [];

function harness({popover = 'supported', width = 390, height = 844, loading = false} = {}) {
  const microtasks = [], observers = [], timers = new Map();
  let time = 0, timerID = 0, optionNodes = 0, innerHTMLWrites = 0, attributeWrites = 0, mutations = 0;
  class Event {
    constructor(type, init = {}) {Object.assign(this, {type, bubbles: false, cancelable: true, defaultPrevented: false}, init);}
    preventDefault() {if (this.cancelable) this.defaultPrevented = true;}
    stopPropagation() {this.stopped = true;}
    stopImmediatePropagation() {this.stopped = true; this.immediate = true;}
    composedPath() {const result = []; for (let node = this.target; node; node = node.parentNode) result.push(node); return [...result, window];}
  }
  class Target {
    constructor() {this.listeners = new Map();}
    addEventListener(type, callback, options = {}) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push({callback, capture: options === true || Boolean(options.capture), once: Boolean(options.once)});
    }
    removeEventListener(type, callback) {this.listeners.set(type, (this.listeners.get(type) ?? []).filter(item => item.callback !== callback));}
    dispatchEvent(event) {
      if (!event.target) event.target = this;
      const path = [];
      for (let node = this; node; node = node.parentNode) path.push(node);
      if (path.at(-1) === document) path.push(window);
      const deliver = (node, capture) => {
        event.currentTarget = node;
        for (const item of [...(node.listeners.get(event.type) ?? [])]) {
          if (item.capture !== capture) continue;
          item.callback.call(node, event);
          if (item.once) node.removeEventListener(event.type, item.callback);
          if (event.immediate) break;
        }
      };
      for (const node of [...path].reverse()) {deliver(node, true); if (event.stopped) break;}
      if (!event.stopped) deliver(this, false);
      if (event.bubbles && !event.stopped) for (const node of path.slice(1)) {deliver(node, false); if (event.stopped) break;}
      return !event.defaultPrevented;
    }
  }
  function mutation(target, type = 'attributes', attributeName) {
    mutations++;
    for (const observer of observers) {
      if (!observer.target || !(target === observer.target || (observer.options.subtree && observer.target.contains(target)))) continue;
      if (!observer.options[type]) continue;
      if (type === 'attributes' && observer.options.attributeFilter && !observer.options.attributeFilter.includes(attributeName)) continue;
      observer.pending.push({target, type, attributeName});
      if (!observer.scheduled) {observer.scheduled = true; microtasks.push(() => {observer.scheduled = false; const records = observer.pending.splice(0); if (records.length) observer.callback(records, observer);});}
    }
  }
  function matches(node, selector) {
    selector = selector.trim();
    const negations = [...selector.matchAll(/:not\(([^)]+)\)/g)].map(match => match[1]);
    selector = selector.replace(/:not\([^)]+\)/g, '');
    if (negations.some(part => matches(node, part))) return false;
    if (selector.includes(':popover-open')) {if (!node.popoverOpen) return false; selector = selector.replace(':popover-open', '');}
    if (selector.includes(':disabled')) {if (!node.disabled) return false; selector = selector.replace(':disabled', '');}
    if (selector.includes(':checked')) {if (!node.selected && !node.checked) return false; selector = selector.replace(':checked', '');}
    const tag = selector.match(/^[a-z][a-z0-9-]*/i)?.[0];
    if (tag && node.tagName !== tag.toUpperCase()) return false;
    const id = selector.match(/#([\w-]+)/)?.[1];
    if (id && node.id !== id) return false;
    for (const name of [...selector.matchAll(/\.([\w-]+)/g)].map(match => match[1])) if (!node.classList.contains(name)) return false;
    for (const match of selector.matchAll(/\[([^\]=\s]+)(?:\s*=\s*["']?([^\]"']*)["']?)?\]/g)) {
      if (!node.hasAttribute(match[1])) return false;
      if (match[2] !== undefined && node.getAttribute(match[1]) !== match[2]) return false;
    }
    return node.nodeType === 1;
  }
  function selected(node, selector) {return selector.split(',').some(part => matches(node, part));}
  class Element extends Target {
    constructor(tag = 'div') {
      super(); this.tagName = tag.toUpperCase(); this.nodeName = this.tagName; this.nodeType = tag === '#text' ? 3 : 1;
      this.children = []; this.attributes = new Map(); this.ownerDocument = document; this.parentNode = null; this._text = ''; this._value = ''; this._selected = false;
      this._hidden = false; this._disabled = false; this._open = false; this._required = false; this._className = ''; this._rect = null; this.style = {setProperty(name, value) {this[name] = String(value);}, removeProperty(name) {delete this[name];}};
      this._dataset = {};
      this.dataset = new Proxy(this._dataset, {set: (target, key, value) => {target[key] = String(value); const name = 'data-' + key.replace(/[A-Z]/g, letter => '-' + letter.toLowerCase()); attributeWrites++; this.attributes.set(name, String(value)); mutation(this, 'attributes', name); return true;}});
      this.classList = {
        contains: name => this._className.split(/\s+/).includes(name),
        add: (...names) => {this.className = [...new Set([...this._className.split(/\s+/).filter(Boolean), ...names])].join(' ');},
        remove: (...names) => {this.className = this._className.split(/\s+/).filter(name => !names.includes(name)).join(' ');},
        toggle: (name, force) => {const add = force ?? !this.classList.contains(name); this.classList[add ? 'add' : 'remove'](name); return add;},
      };
      if (popover !== 'unsupported') {
        this.popover = null;
        this.showPopover = () => {
          if (popover === 'throws') throw new Error('synthetic popover unavailable');
          this.popoverOpen = true; this.dispatchEvent(new Event('toggle', {newState: 'open', oldState: 'closed'}));
        };
        this.hidePopover = () => {this.popoverOpen = false; this.dispatchEvent(new Event('toggle', {newState: 'closed', oldState: 'open'}));};
      }
    }
    get id() {return this.getAttribute('id') ?? '';}
    set id(value) {this.setAttribute('id', value);}
    get name() {return this.getAttribute('name') ?? '';}
    set name(value) {this.setAttribute('name', value);}
    get htmlFor() {return this.getAttribute('for') ?? '';}
    set htmlFor(value) {this.setAttribute('for', value);}
    get className() {return this._className;}
    set className(value) {this._className = String(value); this.attributes.set('class', this._className); mutation(this, 'attributes', 'class');}
    get hidden() {return this._hidden;}
    set hidden(value) {this._hidden = Boolean(value); if (this._hidden) this.attributes.set('hidden', ''); else this.attributes.delete('hidden'); mutation(this, 'attributes', 'hidden');}
    get disabled() {return this._disabled;}
    set disabled(value) {this._disabled = Boolean(value); if (this._disabled) this.attributes.set('disabled', ''); else this.attributes.delete('disabled'); mutation(this, 'attributes', 'disabled');}
    get required() {return this._required;}
    set required(value) {this._required = Boolean(value); if (this._required) this.attributes.set('required', ''); else this.attributes.delete('required'); mutation(this, 'attributes', 'required');}
    get open() {return this._open;}
    set open(value) {this._open = Boolean(value); if (this._open) this.attributes.set('open', ''); else this.attributes.delete('open'); mutation(this, 'attributes', 'open');}
    get selected() {return this._selected;}
    set selected(value) {
      this._selected = Boolean(value);
      if (value) {const select = this.closest('select'); if (select) {for (const option of select.options) if (option !== this) option._selected = false; select._value = this.value;}}
    }
    get value() {if (this.tagName === 'SELECT') return this.options[this.selectedIndex]?.value ?? ''; return this._value;}
    set value(value) {
      this._value = String(value);
      if (this.tagName === 'SELECT') {let found = false; for (const option of this.options) {option._selected = !found && option.value === this._value; if (option._selected) found = true;}}
    }
    get selectedIndex() {return this.tagName === 'SELECT' ? this.options.findIndex(option => option.selected) : -1;}
    set selectedIndex(index) {for (const [position, option] of this.options.entries()) option._selected = position === index; this._value = this.options[index]?.value ?? '';}
    get options() {return this.querySelectorAll('option');}
    get selectedOptions() {return this.options.filter(option => option.selected);}
    get labels() {return document.querySelectorAll('label').filter(label => label.hasAttribute('for') ? label.getAttribute('for') === this.id : label.contains(this));}
    get label() {return this.getAttribute('label') ?? this.textContent;}
    set label(value) {this.setAttribute('label', value);}
    get text() {return this.textContent;}
    get parentElement() {return this.parentNode?.nodeType === 1 ? this.parentNode : null;}
    get childNodes() {return this.children;}
    get firstChild() {return this.children[0] ?? null;}
    get firstElementChild() {return this.children.find(node => node.nodeType === 1) ?? null;}
    get nextSibling() {const children = this.parentNode?.children ?? []; return children[children.indexOf(this) + 1] ?? null;}
    get nextElementSibling() {let node = this.nextSibling; while (node?.nodeType !== 1 && node) node = node.nextSibling; return node;}
    get isConnected() {return this === document.documentElement || document.documentElement.contains(this);}
    get textContent() {return this._text + this.children.map(node => node.textContent).join('');}
    set textContent(value) {for (const node of this.children) node.parentNode = null; this.children = []; this._text = String(value); mutation(this, 'childList');}
    get innerHTML() {return this.textContent;}
    set innerHTML(_) {innerHTMLWrites++; throw new Error('select labels must use textContent rather than HTML');}
    get offsetWidth() {return this.getBoundingClientRect().width;}
    get offsetHeight() {return this.getBoundingClientRect().height;}
    get scrollHeight() {return this.querySelectorAll('[role="option"]').length * 44 + 12;}
    get clientHeight() {return this.offsetHeight;}
    get form() {return this.closest('form');}
    reset() {this.dispatchEvent(new Event('reset', {bubbles: true})); for (const select of this.querySelectorAll('select')) select.selectedIndex = Math.max(0, select.options.findIndex(option => option.defaultSelected));}
    get multiple() {return this.hasAttribute('multiple');}
    get size() {return Number(this.getAttribute('size') ?? 0);}
    append(...nodes) {for (let node of nodes) {if (typeof node === 'string') node = document.createTextNode(node); if (node.nodeType === 11) {this.append(...node.children); continue;} node.remove(); node.parentNode = this; this.children.push(node);} mutation(this, 'childList');}
    appendChild(node) {this.append(node); return node;}
    prepend(...nodes) {for (const node of nodes.reverse()) {node.remove(); node.parentNode = this; this.children.unshift(node);} mutation(this, 'childList');}
    replaceChildren(...nodes) {this.textContent = ''; this.append(...nodes);}
    insertBefore(node, before) {if (!before) return this.appendChild(node); node.remove(); node.parentNode = this; this.children.splice(this.children.indexOf(before), 0, node); mutation(this, 'childList'); return node;}
    after(...nodes) {for (const node of nodes.reverse()) this.parentNode?.insertBefore(node, this.nextSibling);}
    before(...nodes) {for (const node of nodes) this.parentNode?.insertBefore(node, this);}
    replaceWith(node) {const parent = this.parentNode; if (parent) {parent.insertBefore(node, this); this.remove();}}
    remove() {if (!this.parentNode) return; const parent = this.parentNode; parent.children.splice(parent.children.indexOf(this), 1); this.parentNode = null; mutation(parent, 'childList');}
    setAttribute(name, value) {
      attributeWrites++;
      this.attributes.set(name, String(value));
      if (name === 'role' && value === 'option') optionNodes++;
      if (name === 'class') this._className = String(value);
      if (name === 'hidden') this._hidden = true;
      if (name === 'disabled') this._disabled = true;
      if (name === 'required') this._required = true;
      if (name === 'open') this._open = true;
      if (name.startsWith('data-')) this._dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = String(value);
      mutation(this, 'attributes', name);
    }
    getAttribute(name) {return this.attributes.get(name) ?? null;}
    hasAttribute(name) {return this.attributes.has(name);}
    removeAttribute(name) {attributeWrites++; this.attributes.delete(name); if (name === 'hidden') this._hidden = false; if (name === 'disabled') this._disabled = false; if (name === 'required') this._required = false; if (name === 'open') this._open = false; mutation(this, 'attributes', name);}
    toggleAttribute(name, force) {const add = force ?? !this.hasAttribute(name); if (add) this.setAttribute(name, ''); else this.removeAttribute(name); return add;}
    matches(selector) {return selected(this, selector);}
    closest(selector) {for (let node = this; node?.nodeType === 1; node = node.parentElement) if (node.matches(selector)) return node; return null;}
    contains(target) {for (let node = target; node; node = node.parentNode) if (node === this) return true; return false;}
    querySelectorAll(selector) {
      const result = [];
      const walk = node => {for (const child of node.children) {if (selected(child, selector)) result.push(child); walk(child);}};
      walk(this); return result;
    }
    querySelector(selector) {return this.querySelectorAll(selector)[0] ?? null;}
    cloneNode(deep = false) {const node = document.createElement(this.tagName.toLowerCase()); node._text = this._text; for (const [key, value] of this.attributes) node.setAttribute(key, value); if (deep) for (const child of this.children) node.append(child.cloneNode(true)); return node;}
    getBoundingClientRect() {
      if (this._rect) return {...this._rect, right: this._rect.left + this._rect.width, bottom: this._rect.top + this._rect.height, x: this._rect.left, y: this._rect.top};
      const listbox = this.getAttribute('role') === 'listbox';
      const left = Number.parseFloat(this.style.left) || 16, top = Number.parseFloat(this.style.top) || 80;
      const boxWidth = Number.parseFloat(this.style.width) || 260;
      const boxHeight = listbox ? Math.min(this.scrollHeight, Number.parseFloat(this.style.maxHeight) || 300) : 44;
      return {left, top, width: boxWidth, height: boxHeight, right: left + boxWidth, bottom: top + boxHeight, x: left, y: top};
    }
    getClientRects() {for (let node = this; node?.nodeType === 1; node = node.parentElement) if (node.hidden || (node.tagName === 'DIALOG' && !node.open) || (node.tagName === 'DETAILS' && !node.open)) return []; return this.isConnected ? [this.getBoundingClientRect()] : [];}
    focus() {
      if (this.disabled || !this.getClientRects().length) return;
      const before = document.activeElement;
      if (before === this) return;
      document.activeElement = this;
      before?.dispatchEvent(new Event('focusout', {bubbles: true, relatedTarget: this}));
      this.dispatchEvent(new Event('focusin', {bubbles: true, relatedTarget: before}));
    }
    blur() {document.body.focus();}
    click() {if (!this.disabled) this.dispatchEvent(new Event('click', {bubbles: true}));}
    scrollIntoView() {this.scrolled = true;}
    close() {this.open = false; this.dispatchEvent(new Event('close'));}
    showModal() {this.open = true;}
  }
  const document = new Target();
  document.nodeType = 9; document.readyState = loading ? 'loading' : 'complete'; document.activeElement = null; document.hidden = false;
  document.createElement = tag => new Element(tag); document.createElementNS = (_, tag) => new Element(tag);
  document.createTextNode = text => {const node = new Element('#text'); node.textContent = text; return node;};
  document.createDocumentFragment = () => {const node = new Element('#fragment'); node.nodeType = 11; return node;};
  document.documentElement = new Element('html'); document.documentElement.parentNode = document;
  Object.defineProperties(document.documentElement, {clientWidth: {value: width}, clientHeight: {value: height}});
  document.body = new Element('body'); document.documentElement.append(document.body); document.activeElement = document.body;
  document.querySelectorAll = selector => document.documentElement.querySelectorAll(selector);
  document.querySelector = selector => document.querySelectorAll(selector)[0] ?? null;
  document.getElementById = id => document.querySelectorAll('[id]').find(node => node.id === id) ?? null;
  document.contains = node => document.documentElement.contains(node);
  class MutationObserver {
    constructor(callback) {this.callback = callback; this.pending = []; this.scheduled = false; observers.push(this);}
    observe(target, options) {this.target = target; this.options = options;}
    disconnect() {this.target = null; this.pending = [];}
    takeRecords() {return this.pending.splice(0);}
  }
  class ResizeObserver {constructor(callback) {this.callback = callback;} observe() {} disconnect() {}}
  const window = new Target();
  Object.assign(window, {document, innerWidth: width, innerHeight: height, Event, MutationObserver, ResizeObserver, HTMLElement: Element, HTMLSelectElement: Element,
    setTimeout(callback, delay = 0) {timers.set(++timerID, {callback, at: time + delay}); return timerID;}, clearTimeout: id => timers.delete(id),
    setInterval() {throw new Error('selects must not poll idle controls');}, clearInterval() {},
    requestAnimationFrame(callback) {microtasks.push(() => callback(time)); return 1;}, cancelAnimationFrame() {}, queueMicrotask: callback => microtasks.push(callback),
    getComputedStyle(node) {return {display: node.getClientRects().length ? 'block' : 'none', visibility: 'visible', direction: 'ltr'};},
    visualViewport: Object.assign(new Target(), {width, height, offsetLeft: 0, offsetTop: 0}),
  });
  class Clock extends Date {static now() {return time;}}
  const context = vm.createContext({window, document, Event, MouseEvent: Event, KeyboardEvent: Event, PointerEvent: Event, MutationObserver, ResizeObserver, HTMLElement: Element, HTMLSelectElement: Element, Element,
    Node: {ELEMENT_NODE: 1, TEXT_NODE: 3}, Date: Clock, performance: {now: () => time}, queueMicrotask: window.queueMicrotask,
    setTimeout: window.setTimeout, clearTimeout: window.clearTimeout, setInterval: window.setInterval, clearInterval: window.clearInterval,
    requestAnimationFrame: window.requestAnimationFrame, cancelAnimationFrame: window.cancelAnimationFrame, getComputedStyle: window.getComputedStyle,
    CSS: {escape: value => String(value)}, console,
  });
  function flush() {
    for (let count = 0; microtasks.length && count < 100; count++) {const pending = microtasks.splice(0); for (const callback of pending) callback();}
    assert.equal(microtasks.length, 0, 'mutation refreshes must settle without a loop');
  }
  function makeSelect({id = 'chat', label = 'Chat', options = [['all', 'All archived chats'], ['noa', 'נועה לוי'], ['atlas', 'Atlas team'], ['deborah', 'Deborah']], value = options[0]?.[0] ?? '', disabled = false, parent = document.body, theme = false} = {}) {
    const row = document.createElement('label'); row.setAttribute('for', id); row.append(document.createTextNode(label));
    const select = document.createElement('select'); select.id = id; select.name = id; select.disabled = disabled;
    if (theme) select.setAttribute('data-theme-select', '');
    for (const [value, text, disabled = false] of options) {const option = document.createElement('option'); option.value = value; option.textContent = text; option.disabled = disabled; select.append(option);}
    select.value = value; row.append(select); parent.append(row); return select;
  }
  function start() {vm.runInContext(source, context, {timeout: 5000}); if (loading) {document.readyState = 'interactive'; document.dispatchEvent(new Event('DOMContentLoaded'));} flush(); assert.ok(window.MyWhatsAppSelects, 'public enhancement API');}
  function trigger(select) {const node = document.querySelectorAll('[role="combobox"]').find(node => node.dataset.selectFor === select.id || node.id === `${select.id}-trigger`); assert.ok(node, `trigger for ${select.id}`); return node;}
  function panel(select) {const node = document.getElementById(trigger(select).getAttribute('aria-controls')); assert.ok(node, `listbox for ${select.id}`); assert.equal(node.getAttribute('role'), 'listbox'); return node;}
  function rows(select) {return panel(select).querySelectorAll('[role="option"]');}
  function key(node, key, init = {}) {const event = new Event('keydown', {bubbles: true, key, ...init}); node.dispatchEvent(event); flush(); return event;}
  function change(select, value) {select.value = value; select.dispatchEvent(new Event('change', {bubbles: true})); flush();}
  function open(select) {trigger(select).focus(); trigger(select).click(); flush(); assert.equal(trigger(select).getAttribute('aria-expanded'), 'true');}
  function outside(node = document.body) {node.dispatchEvent(new Event('pointerdown', {bubbles: true})); flush();}
  function advance(delay) {time += delay; for (const [id, timer] of [...timers]) if (timer.at <= time) {timers.delete(id); timer.callback();} flush();}
  return {document, window, makeSelect, start, flush, trigger, panel, rows, key, change, open, outside, advance, Event,
    metrics: () => ({optionNodes, innerHTMLWrites, attributeWrites, mutations, observers: observers.filter(observer => observer.target).length, timers: timers.size}),
  };
}

function check(name, run) {try {run(); cases.push(name);} catch (error) {failures.push({name, error: error.message});}}
const assertClosed = (h, select) => {assert.equal(h.trigger(select).getAttribute('aria-expanded'), 'false'); assert.equal(h.rows(select).length, 0, 'closed panels discard copied option text');};

check('enhancement retains the native form control and announces label plus current value', () => {
  const h = harness(); const form = h.document.createElement('form'); h.document.body.append(form);
  const select = h.makeSelect({id: 'workspace-theme', label: 'Appearance', value: 'system', options: [['system', 'System'], ['light', 'Light'], ['dark', 'Dark']], parent: form, theme: true}); h.start();
  assert.equal(select.id, 'workspace-theme'); assert.equal(select.name, 'workspace-theme'); assert.equal(select.value, 'system'); assert.equal(select.form, form);
  assert.equal(h.trigger(select).getAttribute('aria-label'), 'Appearance: System'); assert.equal(h.trigger(select).getAttribute('aria-haspopup'), 'listbox'); assertClosed(h, select);
});
check('loading and already-ready documents each initialize controls exactly once', () => {
  for (const loading of [false, true]) {const h = harness({loading}); const select = h.makeSelect(); h.start(); h.document.dispatchEvent(new h.Event('DOMContentLoaded')); h.flush(); assert.equal(h.document.querySelectorAll('[role="combobox"]').length, 1); assert.equal(h.trigger(select).type, 'button');}
});
check('disabled native controls cannot open and regain functionality after refresh', () => {
  const h = harness(); const select = h.makeSelect({disabled: true}); h.start(); assert.equal(h.trigger(select).disabled, true); h.trigger(select).click(); assertClosed(h, select);
  select.disabled = false; h.window.MyWhatsAppSelects.refresh(select); h.flush(); assert.equal(h.trigger(select).disabled, false); h.open(select); select.disabled = true; h.flush(); assertClosed(h, select);
});
check('native change and explicit refresh synchronize changed values without changing form authority', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.change(select, 'atlas'); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: Atlas team');
  const events = []; for (const type of ['input', 'change']) select.addEventListener(type, event => events.push(event.type));
  select.value = 'noa'; h.window.MyWhatsAppSelects.refresh(select); h.flush(); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: נועה לוי'); assert.equal(select.value, 'noa'); assert.deepEqual(events, []);
});
check('dynamic option replacements refresh text and preserve the current native selection', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); const replacement = h.document.createElement('option'); replacement.value = 'new'; replacement.textContent = 'New team'; select.replaceChildren(replacement); select.value = 'new'; h.flush();
  assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: New team'); h.open(select); assert.equal(h.rows(select).length, 1); assert.match(h.rows(select)[0].textContent, /New team/);
  replacement.textContent = 'צוות חדש'; h.flush(); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: צוות חדש');
});
check('Hebrew and markup-like option names are rendered as literal text without HTML parsing', () => {
  const h = harness(); const text = 'נועה <img src=x onerror=alert(1)> & צוות'; const select = h.makeSelect({options: [['safe', text]]}); h.start(); h.open(select);
  assert.ok(h.trigger(select).getAttribute('aria-label').includes(text)); assert.ok(h.rows(select)[0].textContent.includes(text)); assert.equal(h.rows(select)[0].querySelectorAll('img,script').length, 0); assert.equal(h.metrics().innerHTMLWrites, 0);
});
check('selected options are announced and mouse selection emits native input then change exactly once', () => {
  const h = harness(); const select = h.makeSelect(); const events = [];
  for (const type of ['input', 'change']) h.document.body.addEventListener(type, event => {if (event.target === select) events.push({type: event.type, value: select.value});}); h.start(); h.open(select);
  assert.equal(h.rows(select)[0].getAttribute('aria-selected'), 'true'); h.rows(select)[2].click(); h.flush(); assert.equal(select.value, 'atlas'); assert.deepEqual(events, [{type: 'input', value: 'atlas'}, {type: 'change', value: 'atlas'}]); assert.equal(h.document.activeElement, h.trigger(select)); assertClosed(h, select);
});
check('selecting the already-selected option emits neither input nor change', () => {
  const h = harness(); const select = h.makeSelect(); const events = []; for (const type of ['input', 'change']) select.addEventListener(type, event => events.push(event.type)); h.start(); h.open(select); h.rows(select)[0].click(); h.flush(); assert.deepEqual(events, []); assertClosed(h, select);
});
check('arrow navigation skips disabled options and changes the native value only on commit', () => {
  const h = harness(); const select = h.makeSelect({options: [['a', 'Alpha'], ['b', 'Beta', true], ['c', 'Charlie'], ['d', 'Delta']]}); h.start(); h.open(select);
  h.key(h.trigger(select), 'ArrowDown'); assert.equal(select.value, 'a'); const active = h.document.getElementById(h.trigger(select).getAttribute('aria-activedescendant')); assert.ok(active.textContent.includes('Charlie'));
  h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'c'); assertClosed(h, select);
});
check('Home and End select the first and last enabled choices on Enter or Space', () => {
  const h = harness(); const select = h.makeSelect({options: [['a', 'Alpha'], ['b', 'Beta'], ['c', 'Charlie'], ['d', 'Disabled', true]]}); h.start(); h.open(select); h.key(h.trigger(select), 'End'); h.key(h.trigger(select), ' '); assert.equal(select.value, 'c');
  h.open(select); h.key(h.trigger(select), 'Home'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'a');
});
check('closed arrow controls open and Enter or Space opens the current selection', () => {
  const h = harness(); const select = h.makeSelect({value: 'atlas'}); h.start(); h.trigger(select).focus(); h.key(h.trigger(select), 'ArrowDown'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'deborah');
  h.key(h.trigger(select), 'ArrowUp'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'atlas');
  for (const key of ['Enter', ' ']) {h.key(h.trigger(select), key); assert.equal(h.trigger(select).getAttribute('aria-expanded'), 'true'); h.key(h.trigger(select), 'Escape'); assert.equal(select.value, 'atlas');}
});
check('typeahead supports Latin and Hebrew names and resets after its timeout', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); h.key(h.trigger(select), 'd'); h.key(h.trigger(select), 'e'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'deborah');
  h.advance(1500); h.open(select); h.key(h.trigger(select), 'נ'); h.key(h.trigger(select), 'ו'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'noa');
});
check('Tab commits the active choice while leaving default focus navigation available', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); h.key(h.trigger(select), 'End'); const event = h.key(h.trigger(select), 'Tab'); assert.equal(event.defaultPrevented, false); assert.equal(select.value, 'deborah'); assertClosed(h, select);
});
check('Escape cancels the active draft, closes the list and restores visible trigger focus', () => {
  const h = harness(); const select = h.makeSelect(); const events = []; for (const type of ['input', 'change']) select.addEventListener(type, event => events.push(event.type)); h.start(); h.open(select); h.key(h.trigger(select), 'End'); const event = h.key(h.trigger(select), 'Escape'); assert.equal(event.defaultPrevented, true); assert.equal(select.value, 'all'); assert.deepEqual(events, []); assert.equal(h.document.activeElement, h.trigger(select)); assertClosed(h, select);
});
check('only one panel is open and outside dismissal never steals focus from another control', () => {
  const h = harness(); const first = h.makeSelect(), second = h.makeSelect({id: 'kind', label: 'Message type'}); const outside = h.document.createElement('button'); h.document.body.append(outside); h.start(); h.open(first); h.open(second); assertClosed(h, first);
  outside.focus(); h.outside(outside); assert.equal(h.document.activeElement, outside); assertClosed(h, second);
});
check('focus leaving a dropdown closes its draft without committing it', () => {
  const h = harness(); const select = h.makeSelect(); const outside = h.document.createElement('button'); h.document.body.append(outside); h.start(); h.open(select); h.key(h.trigger(select), 'End'); outside.focus(); h.flush(); assert.equal(select.value, 'all'); assert.equal(h.document.activeElement, outside); assertClosed(h, select);
});
check('closeAll clears copied names and preserves focus outside the component', () => {
  const h = harness(); const select = h.makeSelect({options: [['person', 'Synthetic private person']]}); const outside = h.document.createElement('button'); h.document.body.append(outside); h.start(); h.open(select); outside.focus(); h.window.MyWhatsAppSelects.closeAll(); h.flush(); assertClosed(h, select); assert.equal(h.document.activeElement, outside); assert.doesNotMatch(h.panel(select).textContent, /Synthetic private person/);
});
check('hidden workspace cleanup clears both open options and copied selected labels', () => {
  const h = harness(); const workspace = h.document.createElement('main'); workspace.id = 'workspace'; h.document.body.append(workspace); const select = h.makeSelect({options: [['person', 'Synthetic private person']], parent: workspace}); h.start(); h.open(select); workspace.hidden = true; h.flush(); assertClosed(h, select);
  assert.doesNotMatch(h.trigger(select).textContent, /Synthetic private person/); assert.doesNotMatch(h.trigger(select).getAttribute('aria-label') ?? '', /Synthetic private person/); assert.equal(select.value, 'person');
  workspace.hidden = false; h.flush(); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: Synthetic private person');
});
check('closing an owner dialog discards popup names without refocusing its hidden trigger', () => {
  const h = harness(); const dialog = h.document.createElement('dialog'); h.document.body.append(dialog); dialog.showModal(); const select = h.makeSelect({options: [['person', 'Synthetic dialog person']], parent: dialog}); const outside = h.document.createElement('button'); h.document.body.append(outside); h.start(); h.open(select); dialog.close(); outside.focus(); h.flush(); assertClosed(h, select);
  assert.equal(h.document.activeElement, outside); assert.doesNotMatch(h.trigger(select).getAttribute('aria-label') ?? '', /Synthetic dialog person/);
});
check('removed controls close and discard their copied options', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); const trigger = h.trigger(select), panel = h.panel(select); select.closest('label').remove(); h.flush(); assert.equal(trigger.getAttribute('aria-expanded'), 'false'); assert.equal(panel.querySelectorAll('[role="option"]').length, 0);
});
check('unsupported or failing popovers leave a working dropdown and commit path', () => {
  for (const popover of ['unsupported', 'throws']) {const h = harness({popover}); const select = h.makeSelect(); h.start(); h.open(select); h.rows(select)[1].click(); h.flush(); assert.equal(select.value, 'noa'); assertClosed(h, select);}
});
check('native disabled options remain visibly unavailable and cannot be selected with a pointer', () => {
  const h = harness(); const select = h.makeSelect({options: [['a', 'Alpha'], ['b', 'Unavailable', true]]}); h.start(); h.open(select); assert.equal(h.rows(select)[1].getAttribute('aria-disabled'), 'true'); h.rows(select)[1].click(); h.flush(); assert.equal(select.value, 'a'); assert.equal(h.trigger(select).getAttribute('aria-expanded'), 'true');
});
check('large contact lists create option nodes only while open and never start idle polling', () => {
  const h = harness(); const select = h.makeSelect({options: Array.from({length: 1000}, (_, index) => [`person-${index}`, `Synthetic person ${index}`])}); h.start(); assert.equal(h.metrics().optionNodes, 0); assertClosed(h, select);
  h.window.MyWhatsAppSelects.refresh(select); h.flush(); assert.equal(h.metrics().optionNodes, 0); h.open(select); assert.equal(h.rows(select).length, 1000); h.window.MyWhatsAppSelects.closeAll(); h.flush(); assertClosed(h, select); h.advance(5000); assertClosed(h, select); assert.equal(h.metrics().optionNodes, 1000);
});
check('narrow viewport placement clamps the list to screen bounds and flips near the bottom', () => {
  const h = harness({width: 320, height: 568}); const select = h.makeSelect(); h.start(); h.trigger(select)._rect = {left: 250, top: 510, width: 180, height: 44}; h.open(select);
  const rect = h.panel(select).getBoundingClientRect(); assert.ok(rect.left >= 0, 'left edge inside viewport'); assert.ok(rect.right <= 320, 'right edge inside viewport'); assert.ok(rect.top >= 0, 'top inside viewport'); assert.ok(rect.bottom <= 510, 'list flips above a low trigger');
});
check('disabled option groups are skipped by keyboard and reject direct selection', () => {
  const h = harness(); const select = h.makeSelect({options: [['a', 'Alpha'], ['c', 'Charlie']]});
  const group = h.document.createElement('optgroup'); group.disabled = true; const blocked = h.document.createElement('option'); blocked.value = 'b'; blocked.textContent = 'Blocked group item'; group.append(blocked); select.insertBefore(group, select.options[1]); h.start(); h.open(select);
  assert.equal(h.rows(select)[1].getAttribute('aria-disabled'), 'true'); h.rows(select)[1].click(); h.flush(); assert.equal(select.value, 'a'); h.key(h.trigger(select), 'ArrowDown'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'c');
});
check('empty and entirely-disabled lists do not invent a selectable value', () => {
  for (const options of [[], [['a', 'Unavailable A', true], ['b', 'Unavailable B', true]]]) {const h = harness(); const select = h.makeSelect({options}); let changes = 0; select.addEventListener('change', () => changes++); h.start(); const original = select.value; h.open(select); for (const key of ['ArrowDown', 'Home', 'End', 'Enter', ' ']) h.key(h.trigger(select), key); assert.equal(select.value, original); assert.equal(changes, 0); assert.equal(h.trigger(select).getAttribute('aria-activedescendant'), null); h.window.MyWhatsAppSelects.closeAll(); assertClosed(h, select);}
});
check('required validation focuses the visible trigger without submitting or changing its value', () => {
  const h = harness(); const select = h.makeSelect(); select.required = true; h.start(); assert.equal(h.trigger(select).getAttribute('aria-required'), 'true'); const event = new h.Event('invalid'); select.dispatchEvent(event); assert.equal(event.defaultPrevented, true); assert.equal(h.document.activeElement, h.trigger(select)); assert.equal(select.value, 'all');
  select.required = false; h.flush(); assert.equal(h.trigger(select).getAttribute('aria-required'), 'false');
});
check('form reset refreshes the trigger after the native default value is restored', () => {
  const h = harness(); const form = h.document.createElement('form'); h.document.body.append(form); const select = h.makeSelect({parent: form}); select.options[0].defaultSelected = true; h.start(); h.change(select, 'atlas'); const events = []; for (const type of ['input', 'change']) select.addEventListener(type, event => events.push(event.type)); form.reset(); h.flush(); assert.equal(select.value, 'all'); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: All archived chats'); assert.deepEqual(events, []);
});
check('multi-select and native list controls remain unenhanced', () => {
  const h = harness(); const multiple = h.makeSelect({id: 'multiple'}); multiple.setAttribute('multiple', ''); const list = h.makeSelect({id: 'list'}); list.setAttribute('size', '4'); h.start(); assert.equal(h.document.querySelectorAll('[role="combobox"]').length, 0); assert.equal(multiple.value, 'all'); assert.equal(list.value, 'all');
});
check('closing details discards its selected label and restores it only when visible again', () => {
  const h = harness(); const details = h.document.createElement('details'); details.open = true; h.document.body.append(details); const select = h.makeSelect({parent: details, options: [['person', 'Synthetic details person']]}); h.start(); h.open(select); details.open = false; h.flush(); assertClosed(h, select); assert.doesNotMatch(h.trigger(select).getAttribute('aria-label') ?? '', /Synthetic details person/); details.open = true; h.flush(); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: Synthetic details person');
});
check('page lifecycle dismissal cancels the draft and clears copied option names', () => {
  for (const type of ['visibilitychange', 'pagehide']) {const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); h.key(h.trigger(select), 'End'); if (type === 'visibilitychange') {h.document.hidden = true; h.document.dispatchEvent(new h.Event(type));} else h.window.dispatchEvent(new h.Event(type)); h.flush(); assert.equal(select.value, 'all'); assertClosed(h, select);}
});
check('native option removal while open replaces a stale active choice before commit', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); h.key(h.trigger(select), 'End'); select.options.at(-1).remove(); h.flush(); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'all'); assert.equal(select.options.some(option => option.value === 'deborah'), false); assertClosed(h, select);
});
check('choosing a duplicate-value option preserves its actual native option identity', () => {
  const h = harness(); const select = h.makeSelect({options: [['same', 'First item'], ['same', 'Second item'], ['other', 'Other item']]}); let changes = 0; select.addEventListener('change', () => changes++); h.start(); h.open(select); h.rows(select)[1].click(); h.flush(); assert.equal(select.value, 'same'); assert.equal(select.selectedIndex, 1); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: Second item'); assert.equal(changes, 1);
});
check('PageDown and PageUp move through a long list without committing until requested', () => {
  const h = harness(); const select = h.makeSelect({options: Array.from({length: 30}, (_, index) => [`person-${index}`, `Person ${index}`])}); h.start(); h.open(select); h.key(h.trigger(select), 'PageDown'); const afterDown = h.document.getElementById(h.trigger(select).getAttribute('aria-activedescendant')); assert.notEqual(afterDown, h.rows(select)[0]); assert.equal(select.value, 'person-0'); h.key(h.trigger(select), 'PageUp'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'person-0');
});
check('hidden options and groups are absent from the popup and keyboard choices', () => {
  const h = harness(); const select = h.makeSelect({options: [['a', 'Alpha'], ['b', 'Hidden option'], ['c', 'Charlie']]}); select.options[1].hidden = true;
  const group = h.document.createElement('optgroup'); group.hidden = true; const hidden = h.document.createElement('option'); hidden.value = 'hidden'; hidden.textContent = 'Hidden group member'; group.append(hidden); select.append(group); h.start(); h.open(select); assert.equal(h.rows(select).length, 2); assert.doesNotMatch(h.panel(select).textContent, /Hidden/); h.key(h.trigger(select), 'ArrowDown'); h.key(h.trigger(select), 'Enter'); assert.equal(select.value, 'c');
});
check('stale clicks cannot select a removed option or change a newly disabled native control', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); const removedRow = h.rows(select)[2]; select.options[2].remove(); removedRow.click(); h.flush(); assert.equal(select.value, 'all');
  const disabledRow = h.rows(select)[1]; select.disabled = true; disabledRow.click(); h.flush(); assert.equal(select.value, 'all'); assertClosed(h, select);
});
check('an owner-screen transition rejects a queued option click and clears copied text', () => {
  const h = harness(); const workspace = h.document.createElement('main'); h.document.body.append(workspace); const select = h.makeSelect({parent: workspace}); h.start(); h.open(select); const queuedRow = h.rows(select)[2]; workspace.hidden = true; queuedRow.click(); h.flush(); assert.equal(select.value, 'all'); assertClosed(h, select); assert.doesNotMatch(h.trigger(select).getAttribute('aria-label') ?? '', /All archived chats/);
});
check('popup scrolling stays open while scrolling an outside region dismisses the draft', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); h.panel(select).dispatchEvent(new h.Event('scroll')); h.flush(); assert.equal(h.trigger(select).getAttribute('aria-expanded'), 'true'); h.document.body.dispatchEvent(new h.Event('scroll')); h.flush(); assertClosed(h, select); assert.equal(select.value, 'all');
});
check('no native selection remains empty through refresh and cancelled navigation', () => {
  for (const clear of [select => {select.selectedIndex = -1;}, select => {select.value = '';}, select => {select.value = 'missing';}]) {
    const h = harness(); const select = h.makeSelect(); h.start(); clear(select); h.window.MyWhatsAppSelects.refresh(select); h.flush(); assert.equal(select.selectedIndex, -1); assert.equal(select.value, ''); assert.equal(select.selectedOptions.length, 0); assert.equal(h.trigger(select).getAttribute('aria-label'), 'Chat: Choose'); h.open(select); h.key(h.trigger(select), 'ArrowDown'); h.key(h.trigger(select), 'Escape'); assert.equal(select.selectedIndex, -1); assert.equal(select.value, ''); assertClosed(h, select);
  }
  const h = harness(); const select = h.makeSelect({options: []}); h.start(); assert.equal(select.selectedIndex, -1); assert.equal(select.value, ''); assert.equal(select.selectedOptions.length, 0);
});
check('removing only the native select releases its connected wrapper without an observer loop', () => {
  const h = harness(); const select = h.makeSelect({options: [['person', 'Synthetic private person']]}); h.start(); h.open(select);
  const wrapper = select.parentElement, trigger = h.trigger(select), panel = h.panel(select), observed = h.metrics().observers;
  select.remove(); h.flush(); assert.equal(wrapper.isConnected, true); assert.equal(trigger.hidden, true); assertClosed(h, select); assert.doesNotMatch(trigger.textContent + trigger.getAttribute('aria-label') + panel.textContent, /Synthetic private person/); assert.equal(h.metrics().observers, observed - 1);
  const before = h.metrics(); h.window.MyWhatsAppSelects.refresh(select); select.dispatchEvent(new h.Event('change', {bubbles: true})); h.window.MyWhatsAppSelects.closeAll(); h.flush(); assert.equal(h.metrics().mutations, before.mutations, 'released components no longer participate in refresh or cleanup'); assert.equal(h.metrics().attributeWrites, before.attributeWrites);
});
check('labels stay associated with the visible styled trigger after native enhancement', () => {
  const h = harness(); const select = h.makeSelect(); const label = select.closest('label'); h.start(); assert.equal(label.htmlFor, h.trigger(select).id); assert.equal(h.document.getElementById(label.htmlFor), h.trigger(select)); assert.equal(select.labels.length, 0); assert.equal(select.id, 'chat');
});
check('a wrapped option click cancels label activation while committing one choice', () => {
  const h = harness(); const select = h.makeSelect(); h.start(); h.open(select); const event = new h.Event('click', {bubbles: true, cancelable: true}); h.rows(select)[1].dispatchEvent(event); h.flush(); assert.equal(event.defaultPrevented, true); assert.equal(select.value, 'noa'); assertClosed(h, select);
});
check('hovering a large list updates at most the previous and active rows without rerendering', () => {
  const h = harness(); const select = h.makeSelect({options: Array.from({length: 1000}, (_, index) => [`person-${index}`, `Synthetic person ${index}`])}); h.start(); h.open(select); const rows = h.rows(select), initial = h.metrics();
  rows[999].dispatchEvent(new h.Event('pointermove', {bubbles: true})); h.flush(); const moved = h.metrics(); assert.ok(moved.attributeWrites - initial.attributeWrites <= 3, 'hover writes stay bounded regardless of list size'); assert.equal(moved.optionNodes, initial.optionNodes); assert.equal(h.document.getElementById(h.trigger(select).getAttribute('aria-activedescendant')), rows[999]); assert.equal(rows[0].dataset.active, 'false'); assert.equal(rows[999].dataset.active, 'true');
  rows[999].dispatchEvent(new h.Event('pointermove', {bubbles: true})); h.flush(); assert.equal(h.metrics().attributeWrites, moved.attributeWrites, 'repeated hover performs no selection writes'); assert.equal(select.value, 'person-0');
});

console.log(JSON.stringify({ok: failures.length === 0, tests: cases.length, cases, failures, scope: 'shipped select component with synthetic DOM/events; no browser layout or live account'}, null, 2));
assert.equal(failures.length, 0, 'all select component scenarios must pass');
