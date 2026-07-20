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
      headers: options.headers || {},
      rawBody: options.body || null,
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
    URL,
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
  form.requestSubmit();
  await settle();
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

async function run() {
  await testChannelToggleUsesStableID();
  await testGroupRefreshSuppressesStaleGeneration();
  await testGroupDeleteSendsVersionAndReconcilesConflict();
  await testGroupDeleteRefusesMissingVersion();
  await testAssignmentReusesNewestGroupVersion();
  await testTimezoneFocusShowsCatalogAndTypingFilters();
  await testDigestRunButtonSubmitsAndPollsTypedOutcomes();
  await testProviderValidationPrecedesNativeConstraints();
  console.log("WebApp regression harness passed: stable IDs, stale generations, newest optimistic versions, timezone catalog, provider validation, digest click and typed outcomes.");
}

run().catch((error) => {
  console.error(`WebApp refresh regression harness failed: ${error.message}`);
  process.exitCode = 1;
});
