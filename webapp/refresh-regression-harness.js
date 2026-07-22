"use strict";

// Deterministic, dependency-free browser harness for the SPA refresh races.
// It evaluates the production app in a tiny DOM and controls every response
// with explicit deferred handles. No HTTP listener or browser service is used.

const fs = require("fs");
const vm = require("vm");

const appSource = fs.readFileSync(require.resolve("./app.js"), "utf8");
const offlineShellSource = fs.readFileSync(require.resolve("./offline.html"), "utf8");
const serviceWorkerSource = fs.readFileSync(require.resolve("./sw.js"), "utf8");

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function visibleText(node) {
  if (!node) return "";
  return String(node.textContent || "") + node.children.map(visibleText).join("");
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
    this.focused = false;
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

  removeAttribute(name) {
    delete this.attributes[name];
    if (name === "id") this.id = "";
    if (name === "class") this.className = "";
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
    if (this.disabled) return;
    this.dispatchEvent({ type: "click" });
    if (this.type === "submit") {
      const form = this.closest("form");
      if (form) form.requestSubmit();
    }
  }

  focus() {
    this.focused = true;
    this.dispatchEvent({ type: "focus" });
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
    if (selector.includes(":focus") && !this.focused) return false;
    return true;
  }
}

class Document {
  constructor() {
    this.documentElement = new Element("html");
    this.body = new Element("body");
    this.activeElement = null;
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
  const options = arguments[0] || {};
  const document = new Document();
  const app = document.createElement("main");
  app.id = "app";
  document.body.appendChild(app);
  const pending = [];
  const requests = [];
  const consoleErrors = [];
  const hooks = {};
  const windowListeners = {};
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
  const testConsole = {
    log: (...args) => console.log(...args),
    warn: (...args) => console.warn(...args),
    error: (...args) => consoleErrors.push(args.map(String).join(" "))
  };

  const fetch = (path, fetchOptions = {}) => {
    const entry = {
      path,
      method: fetchOptions.method || "GET",
      headers: fetchOptions.headers || {},
      rawBody: fetchOptions.body || null,
      body: fetchOptions.body ? JSON.parse(fetchOptions.body) : null,
      context: options.contextName || "default",
      resolve: null,
      reject: null
    };
    requests.push(entry);
    const promise = new Promise((resolve, reject) => {
      entry.resolve = resolve;
      entry.reject = reject;
    });
    pending.push(entry);
    if (options.backend) {
      Promise.resolve()
        .then(() => options.backend(entry))
        .then((result) => {
          entry.resolve(result && result.status !== undefined ? result : response(result));
        })
        .catch((error) => entry.reject(error));
    }
    return promise;
  };

  const window = {
    Telegram: {
      WebApp: {
        initData: options.initData || "deterministic-test-init-data",
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
    addEventListener: (type, listener) => {
      (windowListeners[type] || (windowListeners[type] = [])).push(listener);
    },
    dispatchEvent: (event) => {
      (windowListeners[event.type] || []).slice().forEach((listener) => listener(event));
    },
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
    URL,
    console: testConsole,
    setTimeout: schedule,
    clearTimeout,
    setInterval: repeat,
    clearInterval,
    addEventListener: window.addEventListener,
    dispatchEvent: window.dispatchEvent
  };
  vm.runInNewContext(appSource, context, { filename: "webapp/app.js" });

  const findPending = (path, method = "GET") => {
    const index = pending.findIndex((entry) => entry.path === path && entry.method === method);
    assert(index >= 0, `Missing pending ${method} ${path}`);
    return pending.splice(index, 1)[0];
  };
  const resolve = (entry, payload, status = 200) => entry.resolve(response(payload, status));

  const allFocusable = () => [
    ...document.querySelectorAll("button"),
    ...document.querySelectorAll("input"),
    ...document.querySelectorAll("select"),
    ...document.querySelectorAll("a")
  ].filter((node) => !node.disabled && !node.hidden);
  const focus = (node) => {
    document.querySelectorAll("*").forEach((item) => { item.focused = false; });
    node.focused = true;
    document.activeElement = node;
    node.dispatchEvent({ type: "focus" });
  };
  const pressKey = (key, modifiers = {}) => {
    const active = document.activeElement;
    const event = {
      type: "keydown",
      key,
      shiftKey: Boolean(modifiers.shiftKey),
      preventDefault() { this.defaultPrevented = true; },
      defaultPrevented: false
    };
    window.dispatchEvent(event);
    if (event.defaultPrevented) return;
    if (key === "Tab") {
      const focusable = allFocusable();
      const currentIndex = active ? focusable.indexOf(active) : -1;
      const delta = modifiers.shiftKey ? -1 : 1;
      const nextIndex = currentIndex < 0
        ? (modifiers.shiftKey ? focusable.length - 1 : 0)
        : (currentIndex + delta + focusable.length) % focusable.length;
      if (focusable[nextIndex]) focus(focusable[nextIndex]);
    } else if (key === "Enter" && active) {
      if (active.tagName === "BUTTON") active.click();
      else if (active.closest("form")) active.closest("form").requestSubmit();
    }
  };

  return { document, window, hooks, requests, consoleErrors, findPending, resolve, focus, pressKey };
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

async function testGroupDeleteSendsVersionAndReconcilesConflict() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const groupsTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Группы")
  );
  assert(groupsTab, "Groups tab was not rendered");
  groupsTab.click();
  const groups = app.findPending("/api/groups?with_channels=true");
  app.resolve(groups, [{
    id: 7,
    version: 7,
    telegram_chat_id: "-1007",
    title: "Authoritative before delete",
    assignments: []
  }]);
  await settle();

  const deleteButton = app.document.querySelectorAll("button").find((button) => button.textContent === "Удалить");
  assert(deleteButton, "Group delete action was not rendered");
  deleteButton.click();
  const confirmation = app.document.querySelector('[role="dialog"]');
  assert(confirmation, "Group delete confirmation did not open");
  const confirmButton = confirmation.querySelectorAll("button").find((button) => button.textContent === "Удалить");
  assert(confirmButton, "Group delete confirmation action was not rendered");
  confirmButton.click();

  const deletion = app.findPending("/api/groups/7", "DELETE");
  assert(deletion.rawBody === '{"version":7}', "Group delete body did not contain the loaded positive version");
  assert(deletion.body.version === 7, "Group delete version was not parsed as the loaded positive version");
  assert(deletion.headers["Content-Type"] === "application/json", "Group delete did not use JSON content type");
  assert(deletion.headers["X-Telegram-Init-Data"] === "deterministic-test-init-data", "Group delete did not include authenticated initData");
  assert(app.hooks.findGroup("7").version === 7, "Group delete removed the group before the server response");

  app.resolve(deletion, { error: "stale version" }, 409);
  await settle();
  const refreshed = app.findPending("/api/groups?with_channels=true");
  app.resolve(refreshed, [{
    id: 7,
    version: 8,
    telegram_chat_id: "-1007",
    title: "Authoritative after conflict",
    assignments: []
  }]);
  await settle();
  const recovered = app.hooks.findGroup("7");
  assert(recovered && recovered.version === 8 && recovered.title === "Authoritative after conflict", "Stale conflict did not reconcile authoritative group state");
  assert(
    app.document.querySelectorAll("span").some((span) => span.textContent.includes("Группа изменилась")),
    "Stale conflict did not show a visible conflict result"
  );

  const retryButton = app.document.querySelectorAll("button").find((button) => button.textContent === "Удалить");
  assert(retryButton, "Group delete retry action was not rendered after reconciliation");
  retryButton.click();
  const retryConfirmation = app.document.querySelector('[role="dialog"]');
  retryConfirmation.querySelectorAll("button").find((button) => button.textContent === "Удалить").click();
  const retry = app.findPending("/api/groups/7", "DELETE");
  assert(retry.body.version === 8 && retry.rawBody === '{"version":8}', "Group delete retry reused the stale version");
  app.resolve(retry, null, 204);
  await settle();
  const afterDelete = app.findPending("/api/groups?with_channels=true");
  app.resolve(afterDelete, []);
  await settle();
  await settle();
  assert(!app.hooks.findGroup("7"), "Successful retry did not remove the group after server confirmation");
  assert(
    app.document.querySelectorAll(".empty").some((empty) =>
      empty.children.some((child) => child.textContent === "Нет добавленных групп")
    ),
    "DOM did not show the authoritative empty group state"
  );
}

async function testGroupDeleteRefusesMissingVersion() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const groupsTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Группы")
  );
  groupsTab.click();
  const groups = app.findPending("/api/groups?with_channels=true");
  app.resolve(groups, [{
    id: 9,
    telegram_chat_id: "-1009",
    title: "Missing version",
    assignments: []
  }]);
  await settle();
  app.document.querySelectorAll("button").find((button) => button.textContent === "Удалить").click();
  const confirmation = app.document.querySelector('[role="dialog"]');
  confirmation.querySelectorAll("button").find((button) => button.textContent === "Удалить").click();
  await settle();

  assert(
    !app.requests.some((request) => request.path === "/api/groups/9" && request.method === "DELETE"),
    "Group delete issued a request without a loaded positive version"
  );
  const refreshed = app.findPending("/api/groups?with_channels=true");
  app.resolve(refreshed, [{
    id: 9,
    version: 4,
    telegram_chat_id: "-1009",
    title: "Recovered version",
    assignments: []
  }]);
  await settle();
  assert(app.hooks.findGroup("9").version === 4, "Missing-version delete did not refresh authoritative group data");
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
  const save = form.querySelectorAll("button").find((button) => button.textContent === "Назначить");
  assert(save, "Assignment modal did not render the visible Назначить button");
  assert(save.type === "submit", "Assignment button is not a submit control");
  save.click();
  await settle();
  const mutation = app.findPending("/api/groups/7/channels", "POST");
  assert(mutation.body.version === 5, "Assignment mutation reused a stale optimistic group version");
  assert(String(mutation.body.channel_id) === "11", "Assignment mutation did not post the selected channel ID");
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
  await settle();
  assert(!app.document.querySelector('[role="dialog"]'), "Successful assignment left the modal open");
  assert(
    app.document.querySelectorAll(".toast").some((toast) => visibleText(toast).includes("Каналы назначены.")),
    "Successful assignment did not show visible success feedback"
  );
}

async function testTimezoneFocusShowsCatalogAndTypingFilters() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const settingsTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Настройки")
  );
  assert(settingsTab, "Settings tab was not rendered");
  settingsTab.click();
  const settings = app.findPending("/api/settings");
  app.resolve(settings, {
    digest_time: "21:00",
    timezone: "Europe/Moscow",
    default_model: "openai/gpt-oss-120b",
    version: 3
  });
  await settle();

  const timezoneInput = app.document.querySelector("#settings-timezone");
  const timezoneDropdown = app.document.querySelector('[role="listbox"]');
  assert(timezoneInput && timezoneDropdown, "Timezone field was not rendered");
  timezoneInput.focus();
  const catalogOptions = timezoneDropdown.querySelectorAll('[role="option"]');
  assert(!timezoneDropdown.hidden, "Timezone catalog stayed hidden on focus");
  assert(timezoneInput.value === "Europe/Moscow", "Saved timezone value was not preserved on focus");
  assert(catalogOptions.length > 100, "Focus did not show the complete timezone catalog");
  assert(
    timezoneDropdown.querySelectorAll(".timezone-group").length > 3,
    "Timezone catalog was not grouped by region"
  );

  timezoneInput.value = "Moscow";
  timezoneInput.dispatchEvent({ type: "input" });
  const filteredOptions = timezoneDropdown.querySelectorAll('[role="option"]');
  assert(filteredOptions.length > 0, "Timezone search returned no Moscow matches");
  assert(
    filteredOptions.every((option) => option.textContent.toLowerCase().includes("moscow")),
    "Timezone search did not filter by the explicit query"
  );
  assert(
    filteredOptions.every((option) => !option.textContent.includes("Europe/London")),
    "Timezone search left unrelated entries visible"
  );

  filteredOptions[0].click();
  assert(timezoneInput.value === filteredOptions[0].textContent, "Timezone selection did not update the input");
  assert(timezoneDropdown.hidden, "Timezone catalog stayed open after selection");
}

async function testDigestRunButtonSubmitsAndPollsTypedOutcomes() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const digestTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Тест дайджеста")
  );
  assert(digestTab, "Test Digest tab was not rendered");
  digestTab.click();
  const groups = app.findPending("/api/groups?with_channels=true");
  app.resolve(groups, [{
    id: 7,
    version: 1,
    telegram_chat_id: "-1007",
    title: "Forum",
    is_forum: true,
    assignments: [{ channel_id: 11, username: "alpha" }]
  }]);
  await settle();

  const run = app.document.querySelectorAll("button").find((button) =>
    button.textContent === "Запустить тестовый дайджест"
  );
  const groupSelect = app.document.querySelector("#digest-group");
  assert(run && groupSelect, "Digest form controls were not rendered");
  run.click();
  const validation = app.document.querySelector("#digest-group-error");
  assert(validation && validation.textContent === "Выберите группу.", "Missing group was not validated visibly");
  assert(groupSelect.getAttribute("aria-invalid") === "true", "Missing group did not set aria-invalid");
  assert(!app.document.querySelector('[role="dialog"]'), "Confirmation opened before selecting a group");

  groupSelect.value = "7";
  run.click();
  const confirmation = app.document.querySelector('[role="dialog"]');
  assert(confirmation, "Digest confirmation did not open from the visible button click");
  const confirmButton = confirmation.querySelectorAll("button").find((button) => button.textContent === "Запустить");
  assert(confirmButton, "Digest confirmation action was not rendered");
  confirmButton.click();

  const submission = app.findPending("/api/digest/test", "POST");
  assert(submission.body.group_id === "7", "Digest submission did not send the selected group ID");
  app.resolve(submission, { job_id: "job-1" }, 202);
  await settle();
  const status = app.findPending("/api/digest/status?id=job-1");
  app.resolve(status, {
    stage: "completed",
    outcome: "partial",
    post_count: 2,
    channel_count: 1,
    failed_channels: ["beta"],
    failure_details: ["beta недоступен"]
  });
  await settle();
  assert(app.document.querySelector(".digest-result").textContent.includes("Дайджест отправлен частично"), "Polling did not render the typed partial outcome");

  const outcomes = [
    ["succeeded", "Дайджест отправлен!"],
    ["no_posts", "Нет новых постов"],
    ["partial", "Дайджест отправлен частично"],
    ["all_channels_failed", "Не удалось собрать посты"],
    ["ai_failed", "Ошибка суммаризации"],
    ["delivery_failed", "Ошибка отправки"]
  ];
  for (const [outcome, expectedText] of outcomes) {
    run.click();
    const modal = app.document.querySelector('[role="dialog"]');
    assert(modal, `Confirmation did not open for ${outcome}`);
    modal.querySelectorAll("button").find((button) => button.textContent === "Запустить").click();
    const request = app.findPending("/api/digest/test", "POST");
    app.resolve(request, { job_id: `job-${outcome}` }, 202);
    await settle();
    const result = app.findPending(`/api/digest/status?id=job-${outcome}`);
    app.resolve(result, {
      stage: "completed",
      outcome,
      post_count: outcome === "no_posts" ? 0 : 2,
      channel_count: 1,
      failed_channels: outcome === "partial" || outcome === "all_channels_failed" ? ["alpha"] : [],
      failure_details: outcome === "partial" ? ["alpha недоступен"] : [],
      detail: outcome === "ai_failed" ? "Провайдер недоступен" : outcome === "delivery_failed" ? "Telegram отказал" : ""
    });
    await settle();
    assert(app.document.querySelector(".digest-result").textContent.includes(expectedText), `Typed ${outcome} outcome was not rendered`);
  }
}

async function testProviderValidationPrecedesNativeConstraints() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const providersTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Провайдеры")
  );
  assert(providersTab, "Providers tab was not rendered");
  providersTab.click();
  const providers = app.findPending("/api/providers");
  app.resolve(providers, []);
  await settle();

  const add = app.document.querySelectorAll("button").find((button) => button.textContent === "Добавить провайдера");
  assert(add, "Provider add action was not rendered");
  add.click();
  const form = app.document.querySelector("#provider-name").closest("form");
  assert(form && form.noValidate === true, "Provider form did not disable native validation UI");
  for (const id of ["provider-name", "provider-url", "provider-key", "provider-model"]) {
    assert(app.document.querySelector(`#${id}`).required !== true, `${id} still relies on native required validation`);
  }
  assert(app.document.querySelector("#provider-url").type === "text", "Provider URL still relies on native type=url validation");

  app.document.querySelector("#provider-url").value = "";
  form.requestSubmit();
  for (const id of ["provider-name", "provider-url", "provider-key", "provider-model"]) {
    const input = app.document.querySelector(`#${id}`);
    const error = app.document.querySelector(`#${id}-error`);
    assert(error.textContent === "Обязательное поле.", `${id} empty validation was not rendered`);
    assert(input.getAttribute("aria-invalid") === "true", `${id} empty validation did not set aria-invalid`);
  }
  assert(!app.requests.some((request) => request.path === "/api/providers" && request.method === "POST"), "Empty provider form issued a request");

  const name = app.document.querySelector("#provider-name");
  const base = app.document.querySelector("#provider-url");
  const key = app.document.querySelector("#provider-key");
  const model = app.document.querySelector("#provider-model");
  name.value = "Custom";
  base.value = "ftp://example.com/v1";
  key.value = "secret";
  model.value = "model";
  form.requestSubmit();
  assert(
    app.document.querySelector("#provider-url-error").textContent === "Неверный формат URL. Должен начинаться с https://",
    "Non-HTTPS provider URL did not render the custom URL validation"
  );
  assert(base.getAttribute("aria-invalid") === "true", "Non-HTTPS provider URL did not set aria-invalid");
  assert(!app.requests.some((request) => request.path === "/api/providers" && request.method === "POST"), "Invalid provider URL issued a request");
  assert(name.value === "Custom" && base.value === "ftp://example.com/v1" && key.value === "secret" && model.value === "model", "Invalid provider validation lost draft values");

  base.value = "https://example.com/v1";
  form.requestSubmit();
  const submission = app.findPending("/api/providers", "POST");
  assert(submission.body.base_url === "https://example.com/v1", "Valid provider URL was not submitted");
  app.resolve(submission, { id: 9, version: 1, name: "Custom", base_url: "https://example.com/v1", default_model: "model", has_key: true });
  await settle();
  const refreshed = app.findPending("/api/providers");
  app.resolve(refreshed, [{ id: 9, version: 1, name: "Custom", base_url: "https://example.com/v1", default_model: "model", has_key: true }]);
  await settle();
}

async function testSettingsServerFailurePreservesDraft() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();

  const settingsTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Настройки")
  );
  assert(settingsTab, "Settings tab was not rendered");
  settingsTab.click();
  const settings = app.findPending("/api/settings");
  app.resolve(settings, {
    digest_time: "21:00",
    timezone: "Europe/Moscow",
    default_model: "openai/gpt-oss-120b",
    version: 4
  });
  await settle();

  const time = app.document.querySelector("#settings-time");
  const timezone = app.document.querySelector("#settings-timezone");
  const model = app.document.querySelector("#settings-model");
  const form = time.closest("form");
  time.value = "12:30";
  timezone.value = "UTC";
  model.value = "custom/model";
  [time, timezone, model].forEach((input) => input.dispatchEvent({ type: "input" }));
  form.requestSubmit();
  const submission = app.findPending("/api/settings", "PUT");
  assert(submission.body.version === 4, "Settings save did not send the loaded version");
  app.resolve(submission, { error: "fixture internal error" }, 500);
  await settle();

  assert(
    app.document.querySelectorAll(".toast").some((toast) => visibleText(toast).includes("Не удалось сохранить настройки. Попробуйте позже.")),
    "Settings HTTP 500 did not render the required friendly error"
  );
  assert(
    time.value === "12:30" && timezone.value === "UTC" && model.value === "custom/model",
    "Settings HTTP 500 reverted the entered draft values"
  );
}

async function testAuthenticatedCancelFlowsPreserveStateWithoutMutations() {
  const channelApp = makeApp();
  const channels = channelApp.findPending("/api/channels");
  channelApp.resolve(channels, [{ id: 101, version: 4, username: "fixture_valid", title: "Stable channel", enabled: true }]);
  await settle();
  channelApp.document.querySelectorAll("button").find((button) => button.textContent === "Удалить").click();
  const channelDialog = channelApp.document.querySelector('[role="dialog"]');
  assert(channelDialog, "Channel delete confirmation did not open");
  channelDialog.querySelectorAll("button").find((button) => button.textContent === "Отмена").click();
  assert(!channelApp.document.querySelector('[role="dialog"]'), "Channel cancel left the confirmation open");
  assert(channelApp.hooks.findChannel("101").username === "fixture_valid", "Channel cancel changed visible channel state");
  assert(
    !channelApp.requests.some((request) => request.path === "/api/channels/101" && request.method === "DELETE"),
    "Channel cancel issued a delete request"
  );

  const groupApp = makeApp();
  const groupChannels = groupApp.findPending("/api/channels");
  groupApp.resolve(groupChannels, []);
  await settle();
  groupApp.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Группы")
  ).click();
  const groups = groupApp.findPending("/api/groups?with_channels=true");
  groupApp.resolve(groups, [{
    id: 7,
    version: 3,
    telegram_chat_id: "-1007",
    title: "Stable group",
    assignments: []
  }]);
  await settle();
  groupApp.document.querySelectorAll("button").find((button) => button.textContent === "Удалить").click();
  const groupDialog = groupApp.document.querySelector('[role="dialog"]');
  assert(groupDialog, "Group delete confirmation did not open");
  groupDialog.querySelectorAll("button").find((button) => button.textContent === "Отмена").click();
  assert(!groupApp.document.querySelector('[role="dialog"]'), "Group cancel left the confirmation open");
  assert(groupApp.hooks.findGroup("7").title === "Stable group", "Group cancel changed visible group state");
  assert(
    !groupApp.requests.some((request) => request.path === "/api/groups/7" && request.method === "DELETE"),
    "Group cancel issued a delete request"
  );

  const providerApp = makeApp();
  const providerChannels = providerApp.findPending("/api/channels");
  providerApp.resolve(providerChannels, []);
  await settle();
  providerApp.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Провайдеры")
  ).click();
  const providers = providerApp.findPending("/api/providers");
  providerApp.resolve(providers, [{
    id: 201,
    version: 5,
    name: "Stable provider",
    base_url: "https://validator.local/v1",
    default_model: "validator-model",
    has_key: true,
    is_default: false
  }]);
  await settle();
  providerApp.document.querySelectorAll("button").find((button) => button.textContent === "Редактировать").click();
  const providerDialog = providerApp.document.querySelector('[role="dialog"]');
  assert(providerDialog, "Provider edit form did not open");
  providerApp.document.querySelector("#provider-name").value = "Discarded provider draft";
  providerDialog.querySelectorAll("button").find((button) => button.textContent === "Отмена").click();
  assert(!providerApp.document.querySelector('[role="dialog"]'), "Provider cancel left the edit form open");
  assert(
    visibleText(providerApp.document.querySelector("table")).includes("Stable provider") &&
      !visibleText(providerApp.document.querySelector("table")).includes("Discarded provider draft"),
    "Provider cancel did not preserve the saved provider row"
  );
  assert(
    !providerApp.requests.some((request) => request.path === "/api/providers/201" && request.method === "PUT"),
    "Provider cancel issued an update request"
  );
}

async function testAllAssignedGroupRendersEmptyAssignmentState() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, [
    { id: 101, version: 1, username: "fixture_valid", enabled: true },
    { id: 102, version: 1, username: "fixture_large_01", enabled: true }
  ]);
  await settle();
  app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Группы")
  ).click();
  const groups = app.findPending("/api/groups?with_channels=true");
  app.resolve(groups, [{
    id: 7,
    version: 8,
    telegram_chat_id: "-1007",
    title: "All assigned",
    is_forum: true,
    assignments: [
      { channel_id: 101, username: "fixture_valid" },
      { channel_id: 102, username: "fixture_large_01" }
    ]
  }]);
  await settle();
  app.document.querySelectorAll("button").find((button) => button.textContent === "Назначить каналы").click();
  const detail = app.findPending("/api/groups/7");
  app.resolve(detail, {
    id: 7,
    version: 8,
    telegram_chat_id: "-1007",
    title: "All assigned",
    is_forum: true,
    assignments: [
      { channel_id: 101, username: "fixture_valid" },
      { channel_id: 102, username: "fixture_large_01" }
    ]
  });
  await settle();
  const topics = await findPendingEventually(app, "/api/groups/7/topics");
  app.resolve(topics, [{ message_thread_id: 101, name: "Announcements" }]);
  await settle();
  const modal = app.document.querySelector('[role="dialog"]');
  assert(modal, "All-assigned assignment modal did not open");
  assert(
    visibleText(modal).includes("Все каналы уже назначены этой группе"),
    "All-assigned group did not render its empty assignment state"
  );
  assert(!modal.querySelectorAll("input").some((input) => input.type === "checkbox"), "All-assigned group rendered selectable channels");
  assert(
    !app.requests.some((request) => request.path === "/api/groups/7/channels" && request.method === "POST"),
    "Opening an all-assigned group issued an assignment mutation"
  );
}

async function testLargeChannelFixtureRendersDeterministically() {
  const app = makeApp();
  const fixtureChannels = Array.from({ length: 34 }, (_, index) => ({
    id: 1000 + index,
    version: 1,
    username: `fixture_large_${String(index + 1).padStart(2, "0")}`,
    title: `Large fixture channel ${index + 1}`,
    enabled: index % 4 !== 0
  }));
  const channels = app.findPending("/api/channels");
  app.resolve(channels, fixtureChannels);
  await settle();
  const wrapper = app.document.querySelector(".table-wrap");
  const body = app.document.querySelector("tbody");
  const rows = body ? body.children : [];
  assert(wrapper, "Large channel fixture did not render a scrollable table wrapper");
  assert(rows.length === fixtureChannels.length, `Large channel fixture rendered ${rows.length} rows, want ${fixtureChannels.length}`);
  assert(
    visibleText(rows[0]).includes("@fixture_large_01") &&
      visibleText(rows[rows.length - 1]).includes("@fixture_large_34"),
    "Large channel fixture did not retain deterministic first and last rows"
  );
  assert(
    fixtureChannels.filter((channel) => !channel.enabled).length ===
      rows.filter((row) => row.className.includes("row-muted")).length,
    "Large channel fixture lost deterministic enabled/disabled row states"
  );
}

async function testAvailableGroupPickerUsesAuthenticatedLocalBoundary() {
  const app = makeApp();
  const channels = app.findPending("/api/channels");
  app.resolve(channels, []);
  await settle();
  app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Группы")
  ).click();
  const initialGroups = app.findPending("/api/groups?with_channels=true");
  app.resolve(initialGroups, []);
  await settle();
  const choose = app.document.querySelectorAll("button").find((button) => button.textContent === "Выбрать из списка");
  assert(choose, "Available group picker trigger was not rendered");
  choose.click();
  const available = app.findPending("/api/groups/available");
  assert(
    available.headers["X-Telegram-Init-Data"] === "deterministic-test-init-data",
    "Available group lookup did not use authenticated initData"
  );
  app.resolve(available, [
    { chat_id: "-1007000000101", title: "Validator available forum" },
    { chat_id: "-1007000000102", title: "Validator available second" }
  ]);
  await settle();
  const picker = app.document.querySelector('[role="dialog"]');
  assert(picker && visibleText(picker).includes("Validator available forum"), "Available group fixture did not render in the picker");
  picker.querySelectorAll("button").find((button) => button.textContent.includes("Validator available forum")).click();
  const creation = app.findPending("/api/groups", "POST");
  assert(creation.body.chat_id === "-1007000000101", "Available group picker submitted the wrong chat ID");
  assert(
    creation.headers["X-Telegram-Init-Data"] === "deterministic-test-init-data",
    "Available group selection did not use authenticated initData"
  );
  app.resolve(creation, {}, 201);
  await settle();
  const refreshed = app.findPending("/api/groups?with_channels=true");
  app.resolve(refreshed, [{
    id: 301,
    version: 1,
    telegram_chat_id: "-1007000000101",
    title: "Validator available forum",
    is_forum: true,
    assignments: []
  }]);
  await settle();
  assert(!app.document.querySelector('[role="dialog"]'), "Available group picker stayed open after selection");
  const reconciled = app.hooks.findGroup("301");
  assert(reconciled && reconciled.title === "Validator available forum", "Available group selection did not reconcile the local group list");
  assert(
    app.requests.filter((entry) => entry.path === "/api/groups/available" && entry.method === "GET").length === 1,
    "Available group picker made duplicate discovery requests"
  );
}

function validatorSeededBackend() {
  const state = {
    settings: {
      digest_time: "10:15",
      timezone: "UTC",
      default_model: "validator-model",
      version: 1
    },
    channel: { id: 101, version: 1, username: "fixture_valid", enabled: true }
  };
  const trace = [];
  const backend = (entry) => {
    trace.push({
      context: entry.context,
      method: entry.method,
      path: entry.path,
      headers: { ...entry.headers },
      body: entry.body
    });
    if (entry.path === "/api/channels" && entry.method === "GET") {
      return response([state.channel]);
    }
    if (entry.path === "/api/providers" && entry.method === "GET") {
      return response([{
        id: 201,
        version: 1,
        name: "OpenRouter",
        base_url: "https://openrouter.ai/api/v1",
        default_model: "openai/gpt-oss-120b",
        has_key: true,
        is_default: true
      }]);
    }
    if (entry.path === "/api/settings" && entry.method === "GET") return response(state.settings);
    if (entry.path === "/api/settings" && entry.method === "PUT") {
      if (entry.body.version !== state.settings.version) {
        return response({ error: "Configuration was modified by another session. Please reload and try again." }, 409);
      }
      state.settings = {
        digest_time: entry.body.digest_time,
        timezone: entry.body.timezone,
        default_model: entry.body.default_model,
        version: state.settings.version + 1
      };
      return response(state.settings);
    }
    if (entry.path === "/api/channels" && entry.method === "POST") {
      state.channel = { id: 101, version: state.channel.version + 1, username: entry.body.username.slice(1), enabled: true };
      return response(state.channel, 201);
    }
    return response({ error: `unexpected ${entry.method} ${entry.path}` }, 500);
  };
  return { backend, trace, state };
}

async function testRequestFailuresRenderRecoverableErrorStates() {
  const app = makeApp();
  const initial = app.findPending("/api/channels");
  initial.reject(new TypeError("simulated validator server down"));
  await settle();
  assert(app.document.querySelector(".error-state"), "Network failure did not render a recoverable error state");
  assert(
    visibleText(app.document.querySelector(".error-state")).includes("Не удалось загрузить приложение"),
    "Network failure did not render the friendly application recovery heading"
  );
  assert(
    visibleText(app.document.querySelector(".error-state")).includes("Сервер настроек временно недоступен"),
    "Network failure did not render visible recovery guidance"
  );
  assert(initial.path === "/api/channels" && initial.method === "GET", "Network failure evidence did not capture the API request");
  assert(app.consoleErrors.length === 0, "Network failure unexpectedly produced a console error");

  const retry = app.document.querySelector(".error-state").querySelector("button");
  retry.click();
  const retried = app.findPending("/api/channels");
  app.resolve(retried, [{ id: 101, version: 1, username: "fixture_valid", enabled: true }]);
  await settle();
  assert(app.document.querySelector("table"), "Retry did not restore the channel view");

  const providersTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Провайдеры")
  );
  providersTab.click();
  const providers = app.findPending("/api/providers");
  app.resolve(providers, { error: "default provider fixture unavailable" }, 503);
  await settle();
  assert(app.document.querySelector(".error-state"), "Seeded default-provider failure did not render the error state");
  assert(
    visibleText(app.document.querySelector(".error-state")).includes("default provider fixture unavailable"),
    "Seeded default-provider failure detail was not visible"
  );

  const settingsTab = app.document.querySelectorAll("button").find((button) =>
    button.children.some((child) => child.textContent === "Настройки")
  );
  settingsTab.click();
  const settings = app.findPending("/api/settings");
  app.resolve(settings, { error: "seeded settings fixture unavailable" }, 500);
  await settle();
  assert(app.document.querySelector(".error-state"), "Seeded settings failure did not render the error state");
  assert(
    visibleText(app.document.querySelector(".error-state")).includes("seeded settings fixture unavailable"),
    "Seeded settings failure detail was not visible"
  );
}

async function testOfflineFallbackShellIsLocalAndServedByFailureBoundary() {
  assert(offlineShellSource.includes("Не удалось загрузить приложение"), "Offline shell is missing the recovery heading");
  assert(offlineShellSource.includes("Повторить"), "Offline shell is missing the retry control");
  assert(offlineShellSource.includes("connection refused"), "Offline shell is missing bounded failure detail");
  assert(!offlineShellSource.includes("telegram.org"), "Offline shell contacts Telegram assets");
  assert(!offlineShellSource.includes("https://"), "Offline shell contains an external HTTPS dependency");
  assert(!offlineShellSource.includes("http://"), "Offline shell contains an external HTTP dependency");

  assert(serviceWorkerSource.includes("request.mode !== \"navigate\""), "Service worker handles non-navigation requests");
  assert(serviceWorkerSource.includes("offline.html"), "Service worker does not cache the offline shell");
  assert(serviceWorkerSource.includes("catch"), "Service worker does not handle listener failures");
  assert(!serviceWorkerSource.includes("telegram.org"), "Service worker references an external service");
}

async function testWrongUserContextFailsClosedWithNetworkEvidence() {
  const trace = [];
  const app = makeApp({
    initData: "deterministic-wrong-user-init-data",
    contextName: "wrong-user",
    backend: (entry) => {
      trace.push(entry);
      return response({ error: "unauthorized wrong-user fixture" }, 403);
    }
  });
  const request = app.findPending("/api/channels");
  await settle();
  assert(request.headers["X-Telegram-Init-Data"] === "deterministic-wrong-user-init-data", "Wrong-user context did not send stable initData");
  assert(request.context === "wrong-user", "Wrong-user network evidence lost its context label");
  assert(app.document.querySelector(".readonly-banner"), "Wrong-user 403 did not put the SPA into read-only state");
  assert(app.document.querySelector(".error-state"), "Wrong-user 403 did not render an access error");
  assert(trace.length === 1 && trace[0].path === "/api/channels", "Wrong-user context made unexpected API requests");
}

async function testKeyboardTabEnterEscapeFlowsAreAccessible() {
  const app = makeApp();
  const initial = app.findPending("/api/channels");
  app.resolve(initial, [{ id: 101, version: 1, username: "fixture_valid", enabled: true }]);
  await settle();

  const username = app.document.querySelector("#channel-username");
  assert(username && username.id === "channel-username", "Keyboard flow input lacked a stable accessible ID");
  const add = app.document.querySelector("form").querySelector("button");
  assert(add && add.type === "submit", "Keyboard flow add control was not a submit button");
  app.focus(username);
  const beforeTab = app.document.activeElement;
  app.pressKey("Tab");
  assert(app.document.activeElement && app.document.activeElement !== beforeTab, "Tab did not move focus to the next accessible control");

  app.focus(username);
  username.value = "@keyboard_fixture";
  app.pressKey("Enter");
  const submission = app.findPending("/api/channels", "POST");
  assert(submission.body.username === "@keyboard_fixture", "Enter did not submit the focused channel form");
  app.resolve(submission, { id: 101, version: 2, username: "keyboard_fixture", enabled: true }, 201);
  await settle();
  const refreshed = app.findPending("/api/channels");
  app.resolve(refreshed, [{ id: 101, version: 2, username: "keyboard_fixture", enabled: true }]);
  await settle();
  assert(
    app.document.querySelectorAll(".toast").some((toast) => visibleText(toast).includes("Канал добавлен")),
    "Enter submission did not expose a visible success result"
  );

  const deleteButton = app.document.querySelectorAll("button").find((button) => button.textContent === "Удалить");
  deleteButton.click();
  assert(app.document.querySelector('[role="dialog"]'), "Delete action did not open an accessible dialog");
  app.pressKey("Escape");
  assert(!app.document.querySelector('[role="dialog"]'), "Escape did not close the open dialog");
}

async function testTwoAuthenticatedContextsShareSeededConflictState() {
  const seeded = validatorSeededBackend();
  const first = makeApp({ contextName: "owner-device-1", backend: seeded.backend });
  const second = makeApp({ contextName: "owner-device-2", backend: seeded.backend });
  for (const app of [first, second]) {
    const channels = app.findPending("/api/channels");
    app.resolve(channels, [{ id: 101, version: 1, username: "fixture_valid", enabled: true }]);
    await settle();
    const settingsTab = app.document.querySelectorAll("button").find((button) =>
      button.children.some((child) => child.textContent === "Настройки")
    );
    settingsTab.click();
    const settings = app.findPending("/api/settings");
    app.resolve(settings, seeded.state.settings);
    await settle();
  }

  const firstTime = first.document.querySelector("#settings-time");
  firstTime.value = "11:30";
  firstTime.dispatchEvent({ type: "input" });
  first.document.querySelector("#settings-time").closest("form").requestSubmit();
  await settle();
  const firstPut = seeded.trace.find((request) => request.context === "owner-device-1" && request.method === "PUT");
  assert(firstPut && firstPut.body.version === 1, "First authenticated context did not send the seeded settings version");
  assert(seeded.state.settings.version === 2 && seeded.state.settings.digest_time === "11:30", "First context did not persist the shared seeded settings mutation");

  const secondTime = second.document.querySelector("#settings-time");
  secondTime.value = "12:45";
  secondTime.dispatchEvent({ type: "input" });
  second.document.querySelector("#settings-time").closest("form").requestSubmit();
  await settle();
  const secondPut = seeded.trace.find((request) => request.context === "owner-device-2" && request.method === "PUT");
  assert(secondPut && secondPut.body.version === 1, "Second authenticated context did not retain its stale version");
  assert(
    second.document.querySelectorAll(".toast").some((toast) => visibleText(toast).includes("Настройки изменились")),
    "Stale settings conflict did not render a visible conflict message"
  );
  const refresh = seeded.trace.filter((request) =>
    request.context === "owner-device-2" && request.method === "GET" && request.path === "/api/settings"
  );
  assert(refresh.length >= 2, "Stale conflict did not request authoritative settings recovery");
  await settle();
  assert(second.document.querySelector("#settings-time").value === "11:30", "Second context did not reconcile the first context's authoritative value");
  assert(
    seeded.trace.some((request) => request.context === "owner-device-1") &&
      seeded.trace.some((request) => request.context === "owner-device-2"),
    "Two-context network evidence did not retain both authenticated context labels"
  );
}

async function run() {
  await testChannelToggleUsesStableID();
  await testGroupRefreshSuppressesStaleGeneration();
  await testGroupDeleteSendsVersionAndReconcilesConflict();
  await testGroupDeleteRefusesMissingVersion();
  await testAssignmentReusesNewestGroupVersion();
  await testTimezoneFocusShowsCatalogAndTypingFilters();
  await testDigestRunButtonSubmitsAndPollsTypedOutcomes();
  await testProviderValidationPrecedesNativeConstraints();
  await testSettingsServerFailurePreservesDraft();
  await testAuthenticatedCancelFlowsPreserveStateWithoutMutations();
  await testAllAssignedGroupRendersEmptyAssignmentState();
  await testLargeChannelFixtureRendersDeterministically();
  await testAvailableGroupPickerUsesAuthenticatedLocalBoundary();
  await testRequestFailuresRenderRecoverableErrorStates();
  await testOfflineFallbackShellIsLocalAndServedByFailureBoundary();
  await testWrongUserContextFailsClosedWithNetworkEvidence();
  await testKeyboardTabEnterEscapeFlowsAreAccessible();
  await testTwoAuthenticatedContextsShareSeededConflictState();
  console.log("WebApp regression harness passed: refresh races, request failures, wrong-user auth, keyboard accessibility, seeded conflicts, provider validation, timezone catalog and typed digest outcomes.");
}

run().catch((error) => {
  console.error(`WebApp refresh regression harness failed: ${error.message}`);
  process.exitCode = 1;
});
