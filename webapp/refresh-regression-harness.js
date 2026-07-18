"use strict";

// Deterministic, dependency-free browser harness for the SPA refresh races.
// It evaluates the production app in a tiny DOM and controls every response
// with explicit deferred handles. No HTTP listener or browser service is used.

const fs = require("fs");
const vm = require("vm");

const appSource = fs.readFileSync(require.resolve("./app.js"), "utf8");

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

class Element {
  constructor(tagName) {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attributes = {};
    this.listeners = {};
    this.style = { setProperty: () => {} };
    this.className = "";
    this.id = "";
    this.type = "";
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this.selected = false;
    this.textContent = "";
  }

  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);
    return child;
  }

  removeChild(child) {
    const index = this.children.indexOf(child);
    if (index >= 0) this.children.splice(index, 1);
    child.parentNode = null;
    return child;
  }

  remove() {
    if (this.parentNode) this.parentNode.removeChild(this);
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
    if (name === "id") this.id = String(value);
    if (name === "class") this.className = String(value);
  }

  getAttribute(name) {
    return this.attributes[name];
  }

  addEventListener(type, listener) {
    (this.listeners[type] || (this.listeners[type] = [])).push(listener);
  }

  dispatchEvent(event) {
    event.target = event.target || this;
    event.preventDefault = event.preventDefault || (() => {});
    (this.listeners[event.type] || []).slice().forEach((listener) => listener(event));
    return true;
  }

  click() {
    if (!this.disabled) this.dispatchEvent({ type: "click" });
  }

  requestSubmit() {
    this.dispatchEvent({ type: "submit" });
  }

  closest(selector) {
    let node = this;
    while (node) {
      if (node.matches(selector)) return node;
      node = node.parentNode;
    }
    return null;
  }

  get firstChild() {
    return this.children[0] || null;
  }

  get nextElementSibling() {
    if (!this.parentNode) return null;
    const index = this.parentNode.children.indexOf(this);
    return this.parentNode.children[index + 1] || null;
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    const result = [];
    const visit = (node) => {
      node.children.forEach((child) => {
        if (child.matches(selector)) result.push(child);
        visit(child);
      });
    };
    visit(this);
    return result;
  }

  matches(selector) {
    if (selector === "*") return true;
    const tagMatch = selector.match(/^[a-z]+/i);
    if (tagMatch && this.tagName !== tagMatch[0].toUpperCase()) return false;
    const idMatch = selector.match(/#([A-Za-z0-9_-]+)/);
    if (idMatch && this.id !== idMatch[1]) return false;
    const classMatches = [...selector.matchAll(/\.([A-Za-z0-9_-]+)/g)];
    if (classMatches.some((match) => !(` ${this.className} `).includes(` ${match[1]} `))) return false;
    const attributeMatches = [...selector.matchAll(/\[([A-Za-z0-9_-]+)=["']([^"']*)["']\]/g)];
    if (attributeMatches.some((match) => this.getAttribute(match[1]) !== match[2])) return false;
    if (selector.includes(":checked") && !this.checked) return false;
    return true;
  }
}

class Document {
  constructor() {
    this.documentElement = new Element("html");
    this.body = new Element("body");
    this.documentElement.appendChild(this.body);
  }

  createElement(tagName) {
    return new Element(tagName);
  }

  getElementById(id) {
    return this.querySelector(`#${id}`);
  }

  querySelector(selector) {
    return this.documentElement.querySelector(selector);
  }

  querySelectorAll(selector) {
    return this.documentElement.querySelectorAll(selector);
  }
}

function response(payload, status = 200) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: () => null },
    text: () => Promise.resolve(payload === null ? "" : JSON.stringify(payload))
  };
}

async function settle() {
  for (let i = 0; i < 8; i += 1) await Promise.resolve();
}

async function findPendingEventually(app, path, method = "GET") {
  for (let i = 0; i < 20; i += 1) {
    const entry = app.requests.find((request) => request.path === path && request.method === method && request.resolve);
    if (entry) {
      const index = app.requests.indexOf(entry);
      app.requests.splice(index, 1);
      return entry;
    }
    await settle();
  }
  return app.findPending(path, method);
}

function makeApp() {
  const document = new Document();
  const app = document.createElement("main");
  app.id = "app";
  document.body.appendChild(app);
  const pending = [];
  const requests = [];
  const hooks = {};
  const schedule = (callback, delay, ...args) => {
    const timer = setTimeout(callback, delay, ...args);
    if (timer && typeof timer.unref === "function") timer.unref();
    return timer;
  };
  const repeat = (callback, delay, ...args) => {
    const timer = setInterval(callback, delay, ...args);
    if (timer && typeof timer.unref === "function") timer.unref();
    return timer;
  };

  const fetch = (path, options = {}) => {
    const entry = {
      path,
      method: options.method || "GET",
      body: options.body ? JSON.parse(options.body) : null,
      resolve: null,
      reject: null
    };
    requests.push(entry);
    const promise = new Promise((resolve, reject) => {
      entry.resolve = resolve;
      entry.reject = reject;
    });
    pending.push(entry);
    return promise;
  };

  const window = {
    Telegram: {
      WebApp: {
        initData: "deterministic-test-init-data",
        initDataUnsafe: { user: { first_name: "Harness" } },
        colorScheme: "light",
        themeParams: {},
        ready: () => {},
        expand: () => {},
        onEvent: () => {},
        MainButton: { hide: () => {}, show: () => {}, setText: () => {}, onClick: () => {}, offClick: () => {} },
        BackButton: { hide: () => {}, show: () => {}, onClick: () => {} },
        close: () => {}
      }
    },
    __WEBAPP_TEST_HOOKS__: hooks,
    document,
    Intl,
    setTimeout: schedule,
    clearTimeout,
    setInterval: repeat,
    clearInterval,
    addEventListener: () => {},
    location: { reload: () => {} }
  };
  const context = {
    window,
    document,
    fetch,
    AbortController,
    Promise,
    Date,
    JSON,
    Intl,
    console,
    setTimeout: schedule,
    clearTimeout,
    setInterval: repeat,
    clearInterval
  };
  vm.runInNewContext(appSource, context, { filename: "webapp/app.js" });

  const findPending = (path, method = "GET") => {
    const index = pending.findIndex((entry) => entry.path === path && entry.method === method);
    assert(index >= 0, `Missing pending ${method} ${path}`);
    return pending.splice(index, 1)[0];
  };
  const resolve = (entry, payload, status = 200) => entry.resolve(response(payload, status));

  return { document, hooks, requests, findPending, resolve };
}

async function testChannelToggleUsesStableID() {
  const app = makeApp();
  const initial = app.findPending("/api/channels");
  app.resolve(initial, [{ id: 1, version: 1, username: "alpha", enabled: true }]);
  await settle();

  const toggle = app.document.querySelector('button[aria-label="Выключить канал"]');
  assert(toggle, "Channel toggle was not rendered");
  toggle.click();
  const mutation = app.findPending("/api/channels/1", "PATCH");
  assert(mutation.body.version === 1 && mutation.body.enabled === false, "Toggle did not send the initial version");

  const staleRefresh = app.hooks.refresh("channels", true);
  const stale = app.findPending("/api/channels");
  const newestRefresh = app.hooks.refresh("channels", true);
  const newest = app.findPending("/api/channels");
  app.resolve(newest, [{ id: 1, version: 2, username: "alpha", enabled: true }]);
  await settle();
  app.resolve(stale, [{ id: 1, version: 1, username: "alpha", enabled: true }]);
  await settle();
  app.resolve(mutation, { id: 1, version: 3, username: "alpha", enabled: false });
  await Promise.all([staleRefresh, newestRefresh]);
  await settle();

  const channel = app.hooks.findChannel("1");
  assert(channel && String(channel.id) === "1", "Toggle reconciliation lost the stable channel ID");
  assert(channel.enabled === false && channel.version === 3, "Newest toggle response was not reconciled onto the current channel");
}

async function testGroupRefreshSuppressesStaleGeneration() {
  const app = makeApp();
  const firstLoad = app.hooks.refresh("groups", true);
  const first = app.findPending("/api/groups?with_channels=true");
  const secondLoad = app.hooks.refresh("groups", true);
  const second = app.findPending("/api/groups?with_channels=true");
  app.resolve(second, [{ id: 7, version: 3, telegram_chat_id: "-1007", title: "Newest" }]);
  await settle();
  app.resolve(first, [{ id: 7, version: 2, telegram_chat_id: "-1007", title: "Stale" }]);
  await Promise.all([firstLoad, secondLoad]);
  await settle();

  const group = app.hooks.findGroup("7");
  assert(group && group.title === "Newest" && group.version === 3, "Stale group refresh replaced the newest generation");
}

async function testAssignmentReusesNewestGroupVersion() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, [{ id: 11, version: 1, username: "alpha", enabled: true }]);
  await settle();

  const groupLoad = app.hooks.refresh("groups", true);
  const groupList = app.findPending("/api/groups?with_channels=true");
  app.resolve(groupList, [{
    id: 7,
    version: 4,
    telegram_chat_id: "-1007",
    title: "Forum",
    is_forum: true,
    assignments: []
  }]);
  await groupLoad;
  await settle();

  app.hooks.openAssignment(app.hooks.findGroup("7"));
  const authoritative = app.findPending("/api/groups/7");
  app.resolve(authoritative, {
    id: 7,
    version: 4,
    telegram_chat_id: "-1007",
    title: "Forum",
    is_forum: true,
    assignments: []
  });
  await settle();
  const topics = await findPendingEventually(app, "/api/groups/7/topics");
  app.resolve(topics, []);
  await settle();

  const concurrentRefresh = app.hooks.refresh("groups", true);
  const refreshedList = app.findPending("/api/groups?with_channels=true");
  app.resolve(refreshedList, [{
    id: 7,
    version: 5,
    telegram_chat_id: "-1007",
    title: "Forum",
    is_forum: true,
    assignments: []
  }]);
  await concurrentRefresh;
  await settle();

  const checkbox = app.document.querySelectorAll("input").find((input) => input.type === "checkbox");
  assert(checkbox, "Assignment modal did not render a channel choice");
  checkbox.checked = true;
  const form = checkbox.closest("form");
  assert(form, "Assignment channel choice is not inside a form");
  form.requestSubmit();
  const mutation = app.findPending("/api/groups/7/channels", "POST");
  assert(mutation.body.version === 5, "Assignment mutation reused a stale optimistic group version");
  app.resolve(mutation, {});
  await settle();
  const confirmation = app.findPending("/api/groups/7");
  app.resolve(confirmation, {
    id: 7,
    version: 6,
    telegram_chat_id: "-1007",
    title: "Forum",
    is_forum: true,
    assignments: [{ channel_id: 11, username: "alpha" }]
  });
  await settle();
  assert(app.hooks.findGroup("7").version === 6, "Assignment did not apply its authoritative follow-up group");
}

async function run() {
  await testChannelToggleUsesStableID();
  await testGroupRefreshSuppressesStaleGeneration();
  await testAssignmentReusesNewestGroupVersion();
  console.log("WebApp refresh regression harness passed: stable IDs, stale generations, newest optimistic versions.");
}

run().catch((error) => {
  console.error(`WebApp refresh regression harness failed: ${error.message}`);
  process.exitCode = 1;
});
