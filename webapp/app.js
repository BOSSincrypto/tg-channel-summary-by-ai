/* Digest Control is intentionally framework-free so it can be embedded in the Go binary. */
(function () {
  "use strict";

  var app = document.getElementById("app");
  var telegram = window.Telegram && window.Telegram.WebApp;
  var state = {
    tab: "channels",
    data: { channels: [], groups: [], providers: [], settings: null },
    loadedAt: {},
    loading: {},
    errors: {},
    modal: null,
    readonly: false,
    settingsDraft: null,
    pendingChannelToggles: {},
    loadRequests: {},
    loadGenerations: {},
    groupCollectionGeneration: 0,
    groupReloads: {},
    digestJob: null,
    digestTimer: null,
    retryAfter: 0,
    rateLimitTimer: null,
    fatal: false,
    destroyed: false
  };
  var tabs = [
    { id: "channels", label: "Каналы", icon: "📡" },
    { id: "groups", label: "Группы", icon: "👥" },
    { id: "providers", label: "Провайдеры", icon: "🧠" },
    { id: "settings", label: "Настройки", icon: "⚙️" },
    { id: "digest", label: "Тест дайджеста", icon: "🧪" }
  ];
  var timezoneOptions = [
    "Europe/Moscow", "Europe/London", "Europe/Berlin", "Europe/Paris",
    "Asia/Almaty", "Asia/Dubai", "Asia/Tokyo", "Asia/Shanghai",
    "America/New_York", "America/Los_Angeles", "America/Toronto",
    "Australia/Sydney", "UTC"
  ];
  function supportedTimezones() {
    var zones = timezoneOptions.slice();
    if (window.Intl && typeof window.Intl.supportedValuesOf === "function") {
      try {
        zones = window.Intl.supportedValuesOf("timeZone");
      } catch (_) {
        zones = timezoneOptions.slice();
      }
    }
    if (zones.indexOf("UTC") < 0) zones.push("UTC");
    return zones.sort();
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }
  function attr(node, name, value) {
    if (value !== undefined && value !== null) node.setAttribute(name, String(value));
    return node;
  }
  function button(label, className, handler) {
    var node = el("button", "button " + (className || "secondary"), label);
    node.type = "button";
    if (isRateLimited() && /(^|\s)(primary|danger)(\s|$)/.test(className || "")) node.disabled = true;
    if (handler) node.addEventListener("click", handler);
    return node;
  }
  function rateLimitRemaining() {
    return Math.max(0, Math.ceil((state.retryAfter - Date.now()) / 1000));
  }
  function isRateLimited() { return rateLimitRemaining() > 0; }
  function startRateLimitCountdown() {
    if (state.rateLimitTimer) window.clearInterval(state.rateLimitTimer);
    state.rateLimitTimer = window.setInterval(function () {
      if (!isRateLimited()) {
        window.clearInterval(state.rateLimitTimer);
        state.rateLimitTimer = null;
      }
      render();
    }, 1000);
    render();
  }
  function field(label, name, value, type, options) {
    options = options || {};
    var wrap = el("div", "field" + (options.full ? " full" : ""));
    var labelNode = el("label", "", label);
    labelNode.htmlFor = name;
    wrap.appendChild(labelNode);
    var input = el(type === "select" ? "select" : "input");
    input.id = name;
    input.name = name;
    if (type !== "select") input.type = type || "text";
    if (type === "select") {
      (options.choices || []).forEach(function (choice) {
        var option = el("option", "", choice.label || choice);
        option.value = choice.value === undefined ? choice : choice.value;
        if (String(option.value) === String(value || "")) option.selected = true;
        input.appendChild(option);
      });
    } else if (value !== undefined && value !== null) {
      input.value = value;
    }
    if (options.placeholder) input.placeholder = options.placeholder;
    if (options.required) input.required = true;
    if (options.pattern) input.pattern = options.pattern;
    if (options.inputMode) input.inputMode = options.inputMode;
    wrap.appendChild(input);
    if (options.help) wrap.appendChild(el("small", "", options.help));
    var error = el("div", "error-text");
    error.id = name + "-error";
    wrap.appendChild(error);
    return { wrap: wrap, input: input, error: error };
  }
  function pick(object, names, fallback) {
    if (!object) return fallback;
    for (var i = 0; i < names.length; i += 1) {
      if (object[names[i]] !== undefined && object[names[i]] !== null) return object[names[i]];
    }
    return fallback;
  }
  function idOf(item) { return pick(item, ["id", "ID"], ""); }
  function asArray(value) { return Array.isArray(value) ? value : []; }
  function normalizeChannel(item) {
    return {
      id: idOf(item),
      version: pick(item, ["version", "Version"], 0),
      username: String(pick(item, ["username", "Username"], "")),
      title: String(pick(item, ["title", "Title"], "")),
      enabled: Boolean(pick(item, ["enabled", "Enabled"], true)),
      fetchError: String(pick(item, ["fetch_error_message", "FetchErrorMessage"], ""))
    };
  }
  function normalizeGroup(item) {
    var assignments = pick(item, ["assignments", "channels", "Assignments", "Channels"], []);
    var status = String(pick(item, ["status", "Status"], "active"));
    var forumValue = pick(item, ["is_forum", "IsForum"], status === "" || status === "active");
    return {
      id: idOf(item),
      version: pick(item, ["version", "Version"], 1),
      chatId: String(pick(item, ["telegram_chat_id", "TelegramChatID", "chat_id", "chatId"], "")),
      title: String(pick(item, ["title", "Title"], "")),
      status: status,
      isForum: forumValue === true || forumValue === 1 || forumValue === "true",
      assignments: asArray(assignments).map(normalizeAssignment),
      botStatus: String(pick(item, ["bot_status", "BotStatus"], ""))
    };
  }
  function normalizeAssignment(item) {
    return {
      channelId: String(pick(item, ["channel_id", "ChannelID", "channelId", "id", "ID"], "")),
      username: String(pick(item, ["username", "Username"], "")),
      title: String(pick(item, ["title", "Title"], "")),
      topicId: String(pick(item, ["topic_thread_id", "TopicThreadID", "topicThreadId"], ""))
    };
  }
  function normalizeProvider(item) {
    return {
      id: idOf(item),
      version: pick(item, ["version", "Version"], 0),
      name: String(pick(item, ["name", "Name"], "")),
      baseUrl: String(pick(item, ["base_url", "BaseURL"], "")),
      model: String(pick(item, ["default_model", "DefaultModel", "model", "Model"], "")),
      hasKey: Boolean(pick(item, ["has_key", "HasKey"], false)),
      isDefault: Boolean(pick(item, ["is_default", "IsDefault"], false))
    };
  }
  function normalizeSettings(item) {
    item = item || {};
    return {
      digestTime: String(pick(item, ["digest_time", "DigestTime"], "21:00")),
      timezone: String(pick(item, ["timezone", "Timezone"], "Europe/Moscow")),
      model: String(pick(item, ["default_model", "DefaultModel", "model", "Model"], "openai/gpt-oss-120b")),
      version: pick(item, ["version", "Version"], null)
    };
  }
  function setText(node, value) { node.textContent = value === undefined || value === null ? "" : String(value); }
  function showToast(message, kind, persistent) {
    var region = document.querySelector(".toast-region");
    if (!region) {
      region = el("div", "toast-region");
      attr(region, "aria-live", "polite");
      document.body.appendChild(region);
    }
    var toast = el("div", "toast " + (kind || ""));
    var text = el("span", "", message);
    var close = button("×", "ghost toast-close", function () { toast.remove(); });
    attr(close, "aria-label", "Закрыть уведомление");
    toast.appendChild(text);
    toast.appendChild(close);
    region.appendChild(toast);
    if (!persistent) window.setTimeout(function () { if (toast.parentNode) toast.remove(); }, 5000);
  }
  function haptic(type) {
    if (!telegram || !telegram.HapticFeedback) return;
    if (telegram.HapticFeedback.notificationOccurred && (type === "error" || type === "success")) {
      telegram.HapticFeedback.notificationOccurred(type);
    } else if (telegram.HapticFeedback.impactOccurred) {
      telegram.HapticFeedback.impactOccurred("light");
    }
  }
  function setError(tab, message) {
    state.errors[tab] = message;
    showToast(message, "error", true);
    haptic("error");
    render();
  }
  function clearError(tab) {
    delete state.errors[tab];
  }
  function apiErrorMessage(error) {
    return error && error.message ? error.message : "Что-то пошло не так. Попробуйте ещё раз.";
  }
  function authHeaders() {
    return { "Content-Type": "application/json", "X-Telegram-Init-Data": telegram.initData || "" };
  }
  function api(path, options) {
    options = options || {};
    var controller = options.controller || new AbortController();
    var timedOut = false;
    var timer = window.setTimeout(function () { timedOut = true; controller.abort(); }, 30000);
    var headers = options.headers || authHeaders();
    return fetch(path, {
      method: options.method || "GET",
      headers: headers,
      body: options.body,
      signal: controller.signal
    }).then(function (response) {
      window.clearTimeout(timer);
      if (response.status === 401 || response.status === 403) {
        state.readonly = true;
        throw new Error("Сессия истекла или доступ запрещён. Перезапустите бота через /start.");
      }
      if (response.status === 429) {
        var retryAfter = response.headers.get("Retry-After") || "5";
        var retrySeconds = Number(retryAfter);
        if (!isFinite(retrySeconds) || retrySeconds < 0) retrySeconds = 5;
        retrySeconds = Math.ceil(retrySeconds);
        state.retryAfter = Date.now() + (retrySeconds * 1000);
        startRateLimitCountdown();
        var rateError = new Error("Слишком много запросов. Подождите " + retrySeconds + " секунд.");
        rateError.retryAfter = retrySeconds;
        haptic("error");
        throw rateError;
      }
      if (!response.ok) {
        return response.text().then(function (body) {
          var message = "";
          var payload = null;
          try { payload = JSON.parse(body); message = payload.error || payload.message; } catch (_) { payload = null; message = ""; }
          var apiError = new Error(message || "Ошибка сервера (" + response.status + ")");
          apiError.status = response.status;
          if (payload && payload.field) apiError.field = payload.field;
          throw apiError;
        });
      }
      if (response.status === 204) return null;
      return response.text().then(function (body) {
        if (!body) return null;
        try { return JSON.parse(body); } catch (_) { return body; }
      });
    }).catch(function (error) {
      window.clearTimeout(timer);
      if (error.name === "AbortError") {
        if (timedOut) throw new Error("Превышено время ожидания. Проверьте соединение и повторите.");
        var cancelled = new Error("Запрос отменён.");
        cancelled.cancelled = true;
        throw cancelled;
      }
      throw error;
    });
  }
  function mutation(path, method, payload) {
    if (state.readonly) return Promise.reject(new Error("Сессия истекла. Доступно только чтение."));
    if (Date.now() < state.retryAfter) {
      var remaining = Math.max(1, Math.ceil((state.retryAfter - Date.now()) / 1000));
      return Promise.reject(new Error("Слишком много запросов. Подождите " + remaining + " секунд."));
    }
    return api(path, {
      method: method,
      body: payload === undefined ? undefined : JSON.stringify(payload)
    });
  }
  function load(tab, path, mapper, force) {
    if (!force && state.loadedAt[tab] && Date.now() - state.loadedAt[tab] < 30000) return Promise.resolve();
    if (!force && state.loadRequests[tab]) return state.loadRequests[tab].promise;
    var previous = state.loadRequests[tab];
    if (previous && previous.controller) previous.controller.abort();
    var generation = (state.loadGenerations[tab] || 0) + 1;
    var collectionGeneration = state.groupCollectionGeneration;
    var controller = new AbortController();
    var request = { generation: generation, collectionGeneration: collectionGeneration, controller: controller, promise: null };
    state.loadGenerations[tab] = generation;
    state.loadRequests[tab] = request;
    state.loading[tab] = true;
    clearError(tab);
    render();
    request.promise = api(path, { controller: controller }).then(function (result) {
      if (state.loadGenerations[tab] !== generation) return;
      var mapped = mapper(result);
      if (tab === "groups" && collectionGeneration !== state.groupCollectionGeneration) return;
      if (tab === "groups") mapped = mergeGroupList(mapped, collectionGeneration);
      state.data[tab] = mapped;
      state.loadedAt[tab] = Date.now();
    }).catch(function (error) {
      if (state.loadGenerations[tab] === generation && !error.cancelled) setError(tab, apiErrorMessage(error));
    }).finally(function () {
      if (state.loadGenerations[tab] === generation) {
        state.loading[tab] = false;
        if (state.loadRequests[tab] === request) state.loadRequests[tab] = null;
        render();
      }
    });
    return request.promise;
  }
  function loadChannels(force) { return load("channels", "/api/channels", function (data) { return asArray(data).map(normalizeChannel); }, force); }
  function loadGroups(force) { return load("groups", "/api/groups?with_channels=true", function (data) { return asArray(data).map(normalizeGroup); }, force); }
  function loadProviders(force) { return load("providers", "/api/providers", function (data) { return asArray(data).map(normalizeProvider); }, force); }
  function loadSettings(force) { return load("settings", "/api/settings", normalizeSettings, force); }
  function mergeGroupList(groups, collectionGeneration) {
    var incoming = {};
    groups.forEach(function (group) { incoming[String(group.id)] = group; });
    Object.keys(state.groupReloads).forEach(function (key) {
      var reload = state.groupReloads[key];
      if (!reload || !reload.value || reload.appliedCollectionGeneration <= collectionGeneration) return;
      incoming[key] = reload.value;
    });
    return Object.keys(incoming).map(function (key) { return incoming[key]; });
  }
  function replaceGroup(group) {
    var groups = state.data.groups || [];
    var key = String(group.id);
    var index = groups.findIndex(function (item) { return String(item.id) === key; });
    if (index < 0) groups.push(group);
    else groups[index] = group;
    state.data.groups = groups;
  }
  function findChannel(channelID) {
    var key = String(channelID);
    return (state.data.channels || []).find(function (item) { return String(item.id) === key; }) || null;
  }
  function findGroup(groupID) {
    var key = String(groupID);
    return (state.data.groups || []).find(function (item) { return String(item.id) === key; }) || null;
  }
  function positiveVersion(value, fallback) {
    var version = Number(value);
    if (!isFinite(version) || version <= 0) return fallback || 1;
    return Math.floor(version);
  }
  function reloadGroup(groupID, force) {
    var key = String(groupID);
    var previous = state.groupReloads[key];
    if (!force && previous && previous.promise) return previous.promise;
    if (previous && previous.controller) previous.controller.abort();
    var generation = (previous ? previous.generation : 0) + 1;
    var controller = new AbortController();
    var request = {
      generation: generation,
      controller: controller,
      value: previous && previous.value ? previous.value : null,
      appliedCollectionGeneration: previous && previous.appliedCollectionGeneration ? previous.appliedCollectionGeneration : 0,
      promise: null
    };
    state.groupReloads[key] = request;
    state.groupCollectionGeneration += 1;
    request.promise = api("/api/groups/" + encodeURIComponent(groupID), { controller: controller }).then(function (result) {
      var current = state.groupReloads[key];
      if (!current || current.generation !== generation) return { applied: false };
      var group = normalizeGroup(result);
      current.value = group;
      current.appliedCollectionGeneration = state.groupCollectionGeneration;
      replaceGroup(group);
      state.loadedAt.groups = 0;
      render();
      return { applied: true, group: group };
    }).catch(function (error) {
      var current = state.groupReloads[key];
      if (!current || current.generation !== generation || error.cancelled) return { applied: false };
      setError("groups", apiErrorMessage(error));
      throw error;
    }).finally(function () {
      var current = state.groupReloads[key];
      if (current && current.generation === generation) current.promise = null;
    });
    return request.promise;
  }

  function pageShell() {
    var shell = el("div", "shell");
    var topbar = el("header", "topbar");
    var heading = el("div");
    heading.appendChild(el("div", "eyebrow", "Digest Control"));
    heading.appendChild(el("h1", "", "Центр управления"));
    heading.appendChild(el("p", "subtitle", "Настройте каналы, группы и ежедневные дайджесты"));
    topbar.appendChild(heading);
    var user = pick(telegram.initDataUnsafe, ["user"], null);
    if (user) topbar.appendChild(el("span", "badge", "👋 " + pick(user, ["first_name"], "Администратор")));
    shell.appendChild(topbar);
    var nav = el("nav", "tabs");
    attr(nav, "aria-label", "Разделы приложения");
    tabs.forEach(function (tab) {
      var item = button("", "tab" + (state.tab === tab.id ? " active" : ""), function () { switchTab(tab.id); });
      attr(item, "aria-current", state.tab === tab.id ? "page" : null);
      var icon = el("span", "tab-icon", tab.icon);
      item.appendChild(icon);
      item.appendChild(el("span", "", tab.label));
      nav.appendChild(item);
    });
    shell.appendChild(nav);
    if (state.readonly) shell.appendChild(el("div", "readonly-banner", "Сессия истекла. Данные доступны только для чтения. Перезапустите бота через /start."));
    if (isRateLimited()) {
      shell.appendChild(el("div", "readonly-banner warning-text", "Слишком много запросов. Изменения отключены ещё на " + rateLimitRemaining() + " сек."));
    }
    var content = el("section");
    content.appendChild(renderTab());
    shell.appendChild(content);
    return shell;
  }
  function panel(title, description, action) {
    var outer = el("section", "panel");
    var header = el("div", "panel-header");
    var text = el("div");
    text.appendChild(el("h2", "", title));
    if (description) text.appendChild(el("p", "section-description", description));
    header.appendChild(text);
    if (action) header.appendChild(action);
    outer.appendChild(header);
    var body = el("div", "panel-body");
    outer.appendChild(body);
    return { outer: outer, body: body };
  }
  function loadingState() {
    var node = el("div", "loading");
    node.appendChild(el("div", "spinner"));
    node.appendChild(el("p", "muted", "Загружаем данные…"));
    return node;
  }
  function errorState(tab) {
    var node = el("div", "error-state");
    node.appendChild(el("div", "empty-icon", "⚠️"));
    node.appendChild(el("h3", "", "Не удалось загрузить раздел"));
    node.appendChild(el("p", "", state.errors[tab] || "Попробуйте ещё раз."));
    node.appendChild(button("Повторить", "primary", function () {
      state.loadedAt[tab] = 0;
      refresh(tab, true);
    }));
    return node;
  }
  function emptyState(icon, title, description, action) {
    var node = el("div", "empty");
    node.appendChild(el("div", "empty-icon", icon));
    node.appendChild(el("h3", "", title));
    node.appendChild(el("p", "", description));
    if (action) node.appendChild(action);
    return node;
  }
  function dataBody(tab, renderData) {
    if (state.loading[tab]) return loadingState();
    if (state.errors[tab]) return errorState(tab);
    return renderData();
  }
  function renderChannels() {
    var addUser = field("Username канала", "channel-username", "", "text", {
      placeholder: "@durov", required: true, help: "Можно указать с @ или без него. Допустимы латинские буквы, цифры и _."
    });
    var addButton = button("Добавить", "primary");
    var form = el("form", "inline-form");
    form.appendChild(addUser.wrap);
    form.appendChild(addButton);
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var username = addUser.input.value.trim();
      var normalized = username.charAt(0) === "@" ? username.slice(1) : username;
      if (!/^[A-Za-z0-9_]{5,32}$/.test(normalized)) {
        setText(addUser.error, "Неверный формат username (5–32 символа).");
        addUser.input.setAttribute("aria-invalid", "true");
        return;
      }
      addUser.error.textContent = "";
      addUser.input.removeAttribute("aria-invalid");
      addButton.disabled = true;
      mutation("/api/channels", "POST", { username: "@" + normalized.toLowerCase() }).then(function () {
        addUser.input.value = "";
        showToast("Канал добавлен.", "success");
        state.loadedAt.channels = 0;
        return loadChannels(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); }).finally(function () { addButton.disabled = false; });
    });
    var view = panel("Каналы", "Источники постов для ваших ежедневных дайджестов.", null);
    view.body.appendChild(form);
    var list = state.data.channels;
    if (!list.length) {
      view.body.appendChild(emptyState("📡", "Нет добавленных каналов", "Добавьте канал, чтобы начать сбор постов.", null));
      return view.outer;
    }
    var tableWrap = el("div", "table-wrap");
    var table = el("table", "data-table");
    var head = el("thead");
    var row = el("tr");
    ["Канал", "Статус", "Управление"].forEach(function (label) { row.appendChild(el("th", "", label)); });
    head.appendChild(row); table.appendChild(head);
    var body = el("tbody");
    list.forEach(function (channel) {
      var tr = el("tr", channel.enabled ? "" : "row-muted");
      var identity = el("td");
      identity.appendChild(el("strong", "truncate", "@" + channel.username));
      if (channel.title) identity.appendChild(el("span", "subline truncate", channel.title));
      if (channel.fetchError) identity.appendChild(el("span", "subline warning-text truncate", channel.fetchError));
      tr.appendChild(identity);
      var status = el("td");
      var toggleLabel = el("span", "toggle-label");
      var channelKey = String(channel.id);
      var pendingToggle = state.pendingChannelToggles[channelKey];
      var toggle = button("", "toggle" + (channel.enabled ? " on" : "") + (pendingToggle ? " pending" : ""), function () {
        if (state.readonly || state.pendingChannelToggles[channelKey]) return;
        var before = channel.enabled;
        var beforeVersion = channel.version;
        var request = { before: before, beforeVersion: beforeVersion };
        state.pendingChannelToggles[channelKey] = request;
        channel.enabled = !before;
        render();
        mutation("/api/channels/" + encodeURIComponent(channel.id), "PATCH", { enabled: channel.enabled, version: beforeVersion }).then(function (updated) {
          if (state.pendingChannelToggles[channelKey] !== request) return;
          var current = findChannel(channelKey);
          if (updated && current) Object.assign(current, normalizeChannel(updated));
          delete state.pendingChannelToggles[channelKey];
          if (!current) {
            state.loadedAt.channels = 0;
            return loadChannels(true).then(function () {
              var refreshed = findChannel(channelKey);
              if (state.errors.channels || !refreshed) throw new Error("Не удалось подтвердить актуальное состояние канала.");
              showToast(refreshed && refreshed.enabled ? "Канал включён." : "Канал выключен.", "success");
            });
          }
          showToast(current.enabled ? "Канал включён." : "Канал выключен.", "success");
          render();
        }).catch(function (error) {
          if (state.pendingChannelToggles[channelKey] !== request) return;
          delete state.pendingChannelToggles[channelKey];
          if (error && error.status === 409) {
            showToast("Статус канала изменился. Загружаем актуальное состояние.", "error", true);
            state.loadedAt.channels = 0;
            loadChannels(true);
            return;
          }
          var current = findChannel(channelKey);
          if (current && current.version === beforeVersion) {
            current.enabled = before;
            current.version = beforeVersion;
          } else {
            state.loadedAt.channels = 0;
            loadChannels(true);
          }
          showToast("Не удалось обновить статус канала: " + apiErrorMessage(error), "error", true);
          render();
        });
      });
      toggle.disabled = Boolean(pendingToggle) || state.readonly;
      if (pendingToggle) attr(toggle, "aria-busy", "true");
      attr(toggle, "aria-label", channel.enabled ? "Выключить канал" : "Включить канал");
      toggleLabel.appendChild(toggle);
      toggleLabel.appendChild(el("span", "", channel.enabled ? "Включён" : "Выключен"));
      status.appendChild(toggleLabel); tr.appendChild(status);
      var actions = el("td", "row-actions");
      actions.appendChild(button("Удалить", "danger small", function () { confirmDeleteChannel(channel); }));
      tr.appendChild(actions);
      body.appendChild(tr);
    });
    table.appendChild(body); tableWrap.appendChild(table); view.body.appendChild(tableWrap);
    return view.outer;
  }
  function confirmDeleteChannel(channel) {
    openConfirm("Удалить канал @" + channel.username + "?", "Сводки по этому каналу больше не будут приходить. Если канал используется в группах, он будет отвязан от них.", "Удалить", function () {
      mutation("/api/channels/" + encodeURIComponent(channel.id), "DELETE", { version: channel.version }).then(function () {
        showToast("Канал удалён.", "success"); state.loadedAt.channels = 0; return loadChannels(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
    });
  }
  function renderGroups() {
    var groupField = field("Chat ID группы", "group-chat-id", "", "text", {
      placeholder: "-1001234567890", inputMode: "numeric", help: "Используйте числовой ID супергруппы, обычно начинается с -100."
    });
    var add = button("Добавить", "primary");
    var choose = button("Выбрать из списка", "secondary", openAvailableGroups);
    var form = el("form", "inline-form");
    form.appendChild(groupField.wrap); form.appendChild(add); form.appendChild(choose);
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var chatId = groupField.input.value.trim();
      if (!/^-?\d+$/.test(chatId)) {
        setText(groupField.error, "Chat ID должен быть числом (например, -1001234567890).");
        groupField.input.setAttribute("aria-invalid", "true"); return;
      }
      groupField.error.textContent = ""; groupField.input.removeAttribute("aria-invalid"); add.disabled = true;
      mutation("/api/groups", "POST", { chat_id: chatId }).then(function () {
        groupField.input.value = ""; showToast("Группа добавлена.", "success"); state.loadedAt.groups = 0; return loadGroups(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); }).finally(function () { add.disabled = false; });
    });
    var view = panel("Группы", "Назначьте каналы и топики, куда будут отправляться дайджесты.", null);
    view.body.appendChild(form);
    if (!state.data.groups.length) {
      view.body.appendChild(emptyState("👥", "Нет добавленных групп", "Добавьте бота в группу и укажите её chat_id.", null));
      return view.outer;
    }
    var tableWrap = el("div", "table-wrap");
    var table = el("table", "data-table");
    var head = el("thead"), row = el("tr");
    ["Группа", "Каналы", "Статус", "Управление"].forEach(function (label) { row.appendChild(el("th", "", label)); });
    head.appendChild(row); table.appendChild(head);
    var body = el("tbody");
    state.data.groups.forEach(function (group) {
      var tr = el("tr", "group-row");
      tr.addEventListener("click", function (event) {
        if (event.target.closest("button")) return;
        var details = tr.nextElementSibling;
        if (details) details.hidden = !details.hidden;
      });
      var title = el("td"); title.appendChild(el("strong", "truncate", group.title || "Без названия")); title.appendChild(el("span", "subline", group.chatId)); tr.appendChild(title);
      tr.appendChild(el("td", "", String(group.assignments.length)));
      var status = el("td");
      if (group.status === "ineligible") status.appendChild(el("span", "badge danger", "Не подходит"));
      else if (group.status === "inactive") status.appendChild(el("span", "badge warning", "Неактивна"));
      else if (group.botStatus && group.botStatus !== "administrator") status.appendChild(el("span", "badge warning", "Нужны права"));
      else status.appendChild(el("span", "badge success", "Активна"));
      tr.appendChild(status);
      var actions = el("td", "row-actions");
      actions.appendChild(button("Назначить каналы", "secondary small", function () { openAssignment(group); }));
      actions.appendChild(button("Удалить", "danger small", function () { confirmDeleteGroup(group); }));
      tr.appendChild(actions); body.appendChild(tr);
      var detailRow = el("tr");
      var detailCell = el("td", "details-cell"); detailCell.colSpan = 4;
      detailCell.hidden = false;
      if (!group.assignments.length) detailCell.appendChild(el("span", "muted", "Каналы ещё не назначены."));
      group.assignments.forEach(function (assignment) {
        var assignmentNode = el("div", "assignment");
        var line = el("div"); line.appendChild(el("strong", "", "@" + assignment.username)); if (assignment.title) line.appendChild(el("span", "subline", assignment.title));
        if (group.isForum && assignment.topicId) line.appendChild(el("span", "subline", "Топик: " + assignment.topicId));
        assignmentNode.appendChild(line);
        assignmentNode.appendChild(button("Отвязать", "ghost small", function () {
          openConfirm("Отвязать канал @" + assignment.username + "?", "Канал останется в списке каналов.", "Отвязать", function () {
            mutation("/api/groups/" + encodeURIComponent(group.id) + "/channels/" + encodeURIComponent(assignment.channelId), "DELETE").then(function () {
              return reloadGroup(group.id, true).then(function (result) {
                if (!result.applied) throw new Error("Не удалось подтвердить актуальное состояние группы.");
                showToast("Канал отвязан.", "success");
              });
            }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
          });
        }));
        detailCell.appendChild(assignmentNode);
      });
      detailRow.appendChild(detailCell); body.appendChild(detailRow);
    });
    table.appendChild(body); tableWrap.appendChild(table); view.body.appendChild(tableWrap);
    return view.outer;
  }
  function confirmDeleteGroup(group) {
    openConfirm("Удалить группу " + (group.title || group.chatId) + "?", "Все назначения каналов будут удалены.", "Удалить", function () {
      mutation("/api/groups/" + encodeURIComponent(group.id), "DELETE").then(function () {
        showToast("Группа удалена.", "success"); state.loadedAt.groups = 0; return loadGroups(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
    });
  }
  function openAvailableGroups() {
    api("/api/groups/available").then(function (data) {
      var choices = asArray(data);
      if (!choices.length) { showToast("Нет доступных групп. Добавьте бота в группу.", "warning"); return; }
      openModal("Выберите группу", function (body, close) {
        var list = el("div", "choice-list");
        choices.forEach(function (item) {
          var chatId = String(pick(item, ["chat_id", "telegram_chat_id", "TelegramChatID"], ""));
          var title = String(pick(item, ["title", "Title"], chatId));
          list.appendChild(button(title + " (" + chatId + ")", "secondary", function () {
            mutation("/api/groups", "POST", { chat_id: chatId }).then(function () {
              close(); showToast("Группа добавлена.", "success"); state.loadedAt.groups = 0; return loadGroups(true);
            }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
          }));
        });
        body.appendChild(list);
      });
    }).catch(function (error) {
      if (!error || !error.cancelled) showToast(apiErrorMessage(error), "error", true);
    });
  }
  function openAssignment(group) {
    var groupRequest = reloadGroup(group.id, true).then(function (result) {
      if (!result.applied) {
        var superseded = new Error("Запрос состояния группы заменён более новым.");
        superseded.cancelled = true;
        throw superseded;
      }
      return result.group || findGroup(group.id) || group;
    });
    var topicsRequest = groupRequest.then(function (authoritativeGroup) {
      return authoritativeGroup.isForum
        ? api("/api/groups/" + encodeURIComponent(authoritativeGroup.id) + "/topics").catch(function () { return []; })
        : [];
    });
    Promise.all([loadChannels(false), groupRequest, topicsRequest]).then(function (result) {
      var authoritativeGroup = result[1];
      var assigned = {};
      authoritativeGroup.assignments.forEach(function (item) { assigned[item.channelId] = true; });
      var available = state.data.channels.filter(function (channel) { return !assigned[String(channel.id)]; });
      var topics = asArray(result[2]);
      openModal("Назначить каналы", function (body, close) {
        if (!available.length) {
          body.appendChild(emptyState("✅", "Все каналы уже назначены этой группе", "Добавьте новый канал или отвяжите существующий."));
          return;
        }
        var form = el("form", "stack");
        var list = el("div", "choice-list");
        available.forEach(function (channel) {
          var choice = el("label", "choice");
          var checkbox = el("input"); checkbox.type = "checkbox"; checkbox.value = channel.id;
          choice.appendChild(checkbox); choice.appendChild(el("span", "", "@" + channel.username + (channel.title ? " · " + channel.title : "")));
          list.appendChild(choice);
        });
        form.appendChild(list);
        var topic = null;
        if (authoritativeGroup.isForum) {
          var topicChoices = [{ value: "", label: "Создать новый топик" }].concat(topics.map(function (topicItem) {
            return { value: String(pick(topicItem, ["message_thread_id", "MessageThreadID", "id"], "")), label: String(pick(topicItem, ["name", "Name"], "Топик")) };
          }));
          topic = field("Топик", "assignment-topic", "", "select", { choices: topicChoices, help: "Если топик не выбран, для канала будет создан новый." });
          form.appendChild(topic.wrap);
        }
        var save = button("Назначить", "primary");
        var assignmentActions = el("div", "actions");
        assignmentActions.appendChild(save);
        form.appendChild(assignmentActions);
        form.addEventListener("submit", function (event) {
          event.preventDefault();
          var selected = Array.from(list.querySelectorAll("input:checked")).map(function (item) { return item.value; });
          if (!selected.length) { showToast("Выберите хотя бы один канал.", "warning"); return; }
          save.disabled = true;
          var currentGroup = findGroup(authoritativeGroup.id) || authoritativeGroup;
          var currentVersion = positiveVersion(currentGroup.version, 1);
          Promise.allSettled(selected.map(function (channelId) {
            var payload = { channel_id: channelId, version: currentVersion };
            if (currentGroup.isForum && topic && topic.input.value) payload.topic_thread_id = topic.input.value;
            return mutation("/api/groups/" + encodeURIComponent(currentGroup.id) + "/channels", "POST", payload);
          })).then(function (results) {
            var failed = results.filter(function (result) { return result.status === "rejected"; });
            return reloadGroup(currentGroup.id, true).then(function (reloadResult) {
              if (!reloadResult.applied) throw new Error("Не удалось подтвердить актуальное состояние группы.");
              close();
              if (failed.length) {
                showToast("Часть каналов не удалось назначить. Состояние обновлено.", "error", true);
              } else {
                showToast("Каналы назначены.", "success");
              }
            });
          }).catch(function (error) {
            state.loadedAt.groups = 0;
            showToast(apiErrorMessage(error), "error", true);
          }).finally(function () { save.disabled = false; });
        });
        body.appendChild(form);
      });
    }).catch(function (error) {
      if (!error || !error.cancelled) showToast(apiErrorMessage(error), "error", true);
    });
  }
  function renderProviders() {
    var view = panel("AI-провайдеры", "OpenRouter используется по умолчанию. Секретные ключи никогда не отображаются в интерфейсе.", button("Добавить провайдера", "primary", function () { openProviderForm(); }));
    var list = state.data.providers;
    if (!list.length) {
      view.body.appendChild(emptyState("🧠", "Провайдеры не настроены", "Добавьте совместимый с OpenAI API endpoint.", null));
      return view.outer;
    }
    var tableWrap = el("div", "table-wrap"), table = el("table", "data-table"), head = el("thead"), headRow = el("tr");
    ["Провайдер", "Endpoint", "Модель", "Статус", "Управление"].forEach(function (label) { headRow.appendChild(el("th", "", label)); });
    head.appendChild(headRow); table.appendChild(head);
    var body = el("tbody");
    list.forEach(function (provider) {
      var tr = el("tr");
      var identity = el("td"); identity.appendChild(el("strong", "truncate", provider.name)); identity.appendChild(el("span", "subline", provider.hasKey ? "Ключ сохранён" : "Ключ не задан")); tr.appendChild(identity);
      tr.appendChild(el("td", "truncate", provider.baseUrl));
      tr.appendChild(el("td", "truncate", provider.model));
      var status = el("td"); if (provider.isDefault) status.appendChild(el("span", "badge system", "★ По умолчанию")); else status.appendChild(el("span", "badge", "Дополнительный")); tr.appendChild(status);
      var actions = el("td", "row-actions");
      actions.appendChild(button("Редактировать", "secondary small", function () { openProviderForm(provider); }));
      if (!provider.isDefault && provider.name !== "OpenRouter") {
        actions.appendChild(button("По умолчанию", "ghost small", function () {
          openConfirm("Сделать " + provider.name + " провайдером по умолчанию?", "Новые группы без собственного провайдера будут использовать его.", "Подтвердить", function () {
            mutation("/api/providers/" + encodeURIComponent(provider.id), "PATCH", { name: provider.name, base_url: provider.baseUrl, api_key: "********", default_model: provider.model, is_default: true, version: provider.version }).then(function () {
              showToast("Провайдер выбран по умолчанию.", "success"); state.loadedAt.providers = 0; return loadProviders(true);
            }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
          });
        }));
        actions.appendChild(button("Удалить", "danger small", function () { confirmDeleteProvider(provider); }));
      } else {
        actions.appendChild(el("span", "badge system", "Системный"));
      }
      tr.appendChild(actions); body.appendChild(tr);
    });
    table.appendChild(body); tableWrap.appendChild(table); view.body.appendChild(tableWrap); return view.outer;
  }
  function openProviderForm(provider) {
    provider = provider || { name: "", baseUrl: "https://api.openai.com/v1", apiKey: "", model: "", isDefault: false };
    openModal(provider.id ? "Редактировать провайдера" : "Добавить провайдера", function (body, close) {
      var form = el("form", "stack");
      var name = field("Название", "provider-name", provider.name, "text", { required: true, placeholder: "Мой OpenAI endpoint" });
      var base = field("Base URL", "provider-url", provider.baseUrl, "url", { required: true, placeholder: "https://api.example.com/v1", help: "Только http:// или https://." });
      var key = field("API key", "provider-key", provider.apiKey, "password", { required: !provider.id, placeholder: provider.id ? "Оставьте ********, чтобы сохранить текущий" : "Введите ключ" });
      var model = field("Модель", "provider-model", provider.model, "text", { required: true, placeholder: "openai/gpt-oss-120b" });
      [name, base, key, model].forEach(function (item) { form.appendChild(item.wrap); });
      var actions = el("div", "actions");
      var cancel = button("Отмена", "secondary", close);
      var save = button("Проверить и сохранить", "primary");
      actions.appendChild(cancel); actions.appendChild(save); form.appendChild(actions);
      form.addEventListener("submit", function (event) {
        event.preventDefault();
        var valid = true;
        [[name, name.input.value.trim()], [base, base.input.value.trim()], [model, model.input.value.trim()]].forEach(function (item) {
          item[0].error.textContent = "";
          item[0].input.removeAttribute("aria-invalid");
          if (!item[1]) { item[0].error.textContent = "Обязательное поле."; item[0].input.setAttribute("aria-invalid", "true"); valid = false; }
        });
        try { var parsed = new URL(base.input.value.trim()); if (parsed.protocol !== "https:" && parsed.protocol !== "http:") throw new Error(); }
        catch (_) { base.error.textContent = "Неверный формат URL. Должен начинаться с https:// или http://."; base.input.setAttribute("aria-invalid", "true"); valid = false; }
        if (!valid) return;
        save.disabled = true;
        var payload = { name: name.input.value.trim(), base_url: base.input.value.trim(), api_key: key.input.value.trim(), default_model: model.input.value.trim(), is_default: provider.isDefault };
        if (provider.id) payload.version = provider.version;
        mutation("/api/providers" + (provider.id ? "/" + encodeURIComponent(provider.id) : ""), provider.id ? "PUT" : "POST", payload).then(function () {
          close(); showToast("Провайдер сохранён.", "success"); state.loadedAt.providers = 0; return loadProviders(true);
        }).catch(function (error) {
          var fields = { name: name, base_url: base, api_key: key, default_model: model };
          if (error && error.field && fields[error.field]) {
            fields[error.field].error.textContent = apiErrorMessage(error);
            fields[error.field].input.setAttribute("aria-invalid", "true");
          }
          showToast(apiErrorMessage(error), "error", true);
        }).finally(function () { save.disabled = false; });
      });
      body.appendChild(form);
      name.input.focus();
    });
  }
  function confirmDeleteProvider(provider) {
    openConfirm("Удалить провайдера " + provider.name + "?", "Группы с явным назначением будут переведены на провайдера по умолчанию.", "Удалить", function () {
      mutation("/api/providers/" + encodeURIComponent(provider.id), "DELETE", { version: provider.version }).then(function () {
        showToast("Провайдер удалён.", "success"); state.loadedAt.providers = 0; return loadProviders(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); });
    });
  }
  function renderSettings() {
    var saved = state.data.settings || normalizeSettings({});
    var view = panel("Настройки", "Общие параметры расписания и модели для групп без собственного переопределения.", null);
    var form = el("form", "stack");
    var grid = el("div", "form-grid");
    var time = field("Время дайджеста", "settings-time", saved.digestTime, "time", { required: true, help: "Формат 24 часа, например 21:00." });
    var tz = field("Часовой пояс", "settings-timezone", saved.timezone, "text", { required: true, placeholder: "Начните вводить, например Moscow" });
    var model = field("Модель по умолчанию", "settings-model", saved.model, "text", { required: true, placeholder: "openai/gpt-oss-120b" });
    var timezoneList = supportedTimezones();
    var timezoneDropdown = el("div", "timezone-dropdown");
    attr(timezoneDropdown, "role", "listbox");
    attr(timezoneDropdown, "aria-label", "Доступные часовые пояса");
    function renderTimezoneChoices() {
      while (timezoneDropdown.firstChild) timezoneDropdown.removeChild(timezoneDropdown.firstChild);
      var query = tz.input.value.trim().toLowerCase();
      var groups = {};
      timezoneList.forEach(function (zone) {
        if (query && zone.toLowerCase().indexOf(query) < 0) return;
        var region = zone.indexOf("/") > 0 ? zone.split("/")[0] : "Other";
        if (!groups[region]) groups[region] = [];
        groups[region].push(zone);
      });
      Object.keys(groups).sort().forEach(function (region) {
        var group = el("div", "timezone-group");
        group.appendChild(el("strong", "", region));
        groups[region].forEach(function (zone) {
          var choice = button(zone, "ghost small", function () {
            tz.input.value = zone;
            state.settingsDraft = readSettings(form);
            timezoneDropdown.hidden = true;
          });
          attr(choice, "role", "option");
          group.appendChild(choice);
        });
        timezoneDropdown.appendChild(group);
      });
      attr(timezoneDropdown, "hidden", true);
      timezoneDropdown.hidden = !tz.input.matches(":focus") || !timezoneDropdown.firstChild;
    }
    tz.wrap.appendChild(timezoneDropdown);
    tz.input.addEventListener("focus", renderTimezoneChoices);
    tz.input.addEventListener("input", renderTimezoneChoices);
    tz.input.addEventListener("blur", function () {
      window.setTimeout(function () { timezoneDropdown.hidden = true; }, 120);
    });
    grid.appendChild(time.wrap); grid.appendChild(tz.wrap); grid.appendChild(model.wrap); form.appendChild(grid);
    var actions = el("div", "actions");
    var save = button("Сохранить настройки", "primary");
    var cancel = button("Отмена", "secondary", function () {
      if (settingsChanged(form, saved)) {
        openConfirm("Отменить изменения?", "Введённые значения будут заменены последними сохранёнными.", "Выйти без сохранения", function () {
          discardSettingsAndNavigate("channels");
        });
      } else {
        switchTab("channels");
      }
    });
    actions.appendChild(save); actions.appendChild(cancel); form.appendChild(actions);
    [time.input, tz.input, model.input].forEach(function (input) { input.addEventListener("input", function () { state.settingsDraft = readSettings(form); }); });
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var values = readSettings(form), valid = true;
      [time, tz, model].forEach(function (item) { item.error.textContent = ""; item.input.removeAttribute("aria-invalid"); });
      if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(values.digest_time)) { time.error.textContent = "Формат: ЧЧ:ММ (00:00–23:59)."; time.input.setAttribute("aria-invalid", "true"); valid = false; }
      if (values.timezone !== "UTC" && (!/^[A-Za-z]+(?:[\\/_-][A-Za-z0-9_+.-]+)+$/.test(values.timezone) || values.timezone.length > 80)) { tz.error.textContent = "Укажите корректный часовой пояс IANA."; tz.input.setAttribute("aria-invalid", "true"); valid = false; }
      if (!values.default_model) { model.error.textContent = "Обязательное поле."; model.input.setAttribute("aria-invalid", "true"); valid = false; }
      if (!valid) return;
      save.disabled = true;
      mutation("/api/settings", "PUT", { digest_time: values.digest_time, timezone: values.timezone, default_model: values.default_model, version: saved.version }).then(function () {
        showToast("Настройки сохранены.", "success"); state.loadedAt.settings = 0; state.settingsDraft = null; return loadSettings(true);
      }).catch(function (error) { showToast(apiErrorMessage(error), "error", true); }).finally(function () { save.disabled = false; });
    });
    view.body.appendChild(form); return view.outer;
  }
  function readSettings(form) {
    return {
      digest_time: form.querySelector("#settings-time").value.trim(),
      timezone: form.querySelector("#settings-timezone").value.trim(),
      default_model: form.querySelector("#settings-model").value.trim()
    };
  }
  function settingsChanged(form, saved) {
    var current = readSettings(form);
    return current.digest_time !== saved.digestTime || current.timezone !== saved.timezone || current.default_model !== saved.model;
  }
  function renderDigest() {
    var view = panel("Тестовый дайджест", "Запустите ручную проверку для группы. Прогресс обновляется через обычный HTTP polling.", null);
    var groups = state.data.groups.filter(function (group) { return group.assignments.length > 0; });
    if (!groups.length) {
      view.body.appendChild(emptyState("🧪", "Нет групп с назначенными каналами", "Сначала добавьте группу и назначьте ей каналы.", button("Перейти к группам", "primary", function () { switchTab("groups"); })));
      return view.outer;
    }
    var form = el("form", "stack");
    var choices = [{ value: "", label: "Выберите группу" }].concat(groups.map(function (group) {
      return { value: group.id, label: (group.title || "Без названия") + " (" + group.chatId + ")" };
    }));
    var selected = field("Группа", "digest-group", "", "select", { choices: choices, required: true });
    form.appendChild(selected.wrap);
    var run = button("Запустить тестовый дайджест", "primary");
    var digestActions = el("div", "actions");
    digestActions.appendChild(run);
    form.appendChild(digestActions);
    var progress = el("div", "progress"); progress.hidden = !state.digestJob;
    appendDigestProgress(progress, state.digestJob || { stage: "idle" });
    form.appendChild(progress);
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      if (!selected.input.value) {
        setText(selected.error, "Выберите группу.");
        selected.input.setAttribute("aria-invalid", "true");
        return;
      }
      setText(selected.error, "");
      selected.input.removeAttribute("aria-invalid");
      var group = groups.find(function (item) { return String(item.id) === String(selected.input.value); });
      if (!group) return;
      openConfirm("Запустить дайджест для группы " + (group.title || group.chatId) + "?", "Посты будут собраны, просуммированы и отправлены в группу.", "Запустить", function () {
        run.disabled = true; progress.hidden = false; state.digestJob = { stage: "parsing", detail: "Подготовка…" }; appendDigestProgress(progress, state.digestJob);
        mutation("/api/digest/test", "POST", { group_id: String(group.id) }).then(function (result) {
          var jobId = String(pick(result, ["job_id", "JobID", "id", "ID"], ""));
          if (!jobId) {
            state.digestJob = normalizeDigestResult(result);
            appendDigestProgress(progress, state.digestJob);
            run.disabled = false;
            if (state.digestJob.outcome === "succeeded") showToast("Дайджест отправлен.", "success");
            else if (state.digestJob.outcome) showToast(digestOutcomeText(state.digestJob), "error", true);
            return;
          }
          pollDigest(jobId, progress, run);
        }).catch(function (error) { state.digestJob = { stage: "error", detail: apiErrorMessage(error) }; appendDigestProgress(progress, state.digestJob); run.disabled = false; showToast(apiErrorMessage(error), "error", true); });
      });
    });
    view.body.appendChild(form); return view.outer;
  }
  function normalizeDigestResult(result) {
    var status = String(pick(result, ["status", "Status", "stage", "Stage"], "completed")).toLowerCase();
    var outcome = String(pick(result, ["outcome", "Outcome"], "")).toLowerCase();
    var terminalOutcomes = ["succeeded", "no_posts", "partial", "all_channels_failed", "ai_failed", "delivery_failed"];
    var terminal = terminalOutcomes.indexOf(outcome) >= 0;
    return {
      stage: terminal ? "terminal" : (status.indexOf("error") >= 0 || status.indexOf("fail") >= 0 ? "error" : status),
      outcome: outcome,
      detail: String(pick(result, ["message", "Message", "detail", "Detail"], "")),
      posts: pick(result, ["post_count", "PostCount"], null),
      channels: pick(result, ["channel_count", "ChannelCount"], null),
      failedChannels: asArray(pick(result, ["failed_channels", "FailedChannels"], [])),
      messageId: pick(result, ["message_id", "MessageID"], null),
      messageUrl: String(pick(result, ["message_url", "MessageURL"], "")),
      summariesSaved: Boolean(pick(result, ["summaries_saved", "SummariesSaved"], false)),
      delivered: Boolean(pick(result, ["delivered", "Delivered"], false))
    };
  }
  function digestOutcomeText(job) {
    var posts = job.posts === null || job.posts === undefined ? "" : String(job.posts);
    var channels = job.channels === null || job.channels === undefined ? "" : String(job.channels);
    var failed = job.failedChannels && job.failedChannels.length ? job.failedChannels.join(", ") : "неизвестные каналы";
    switch (job.outcome) {
      case "succeeded":
        return "✅ Дайджест отправлен! " + posts + " постов от " + channels + " каналов.";
      case "no_posts":
        return "ℹ️ Нет новых постов для дайджеста.";
      case "partial":
        return "⚠️ Дайджест отправлен частично. Не удалось обработать: " + failed + ". " + posts + " постов от " + channels + " каналов.";
      case "all_channels_failed":
        return "❌ Не удалось собрать посты. Все каналы недоступны.";
      case "ai_failed":
        return "❌ Ошибка суммаризации: " + (job.detail || "проверьте провайдера AI.") + ".";
      case "delivery_failed":
        return "❌ Ошибка отправки: " + (job.detail || "Telegram не принял сообщение.") + ". " +
          (job.summariesSaved ? "Сводки сохранены, но не доставлены." : "Сводки не доставлены.");
      default:
        return "";
    }
  }
  function appendDigestProgress(node, job) {
    while (node.firstChild) node.removeChild(node.firstChild);
    var stages = [
      ["parsing", "Парсинг каналов…"], ["summarizing", "Суммаризация постов…"], ["sending", "Отправка в группу…"], ["completed", "Готово!"]
    ];
    var current = job.stage || "idle";
    stages.forEach(function (stage, index) {
      var item = el("div", "progress-step");
      if (current === stage[0]) item.className += " active";
      if ((current === "completed" && index < 3) || (current === "sending" && index < 2) || (current === "summarizing" && index < 1)) item.className += " done";
      item.appendChild(el("span", "progress-dot")); item.appendChild(el("span", "", stage[1])); node.appendChild(item);
    });
    if (job.outcome) {
      var outcomeText = digestOutcomeText(job);
      node.appendChild(el("div", "digest-result " + (job.outcome === "succeeded" || job.outcome === "no_posts" ? "success" : (job.outcome === "partial" ? "warning" : "error")), outcomeText));
      if (job.messageId) {
        node.appendChild(el("p", "muted", "Идентификатор сообщения: " + job.messageId));
      }
      if (job.messageUrl) {
        var link = el("a", "subline", "Открыть сообщение в Telegram");
        link.href = job.messageUrl;
        link.target = "_blank";
        link.rel = "noopener";
        node.appendChild(link);
      }
    } else if (job.stage === "error") node.appendChild(el("div", "error-text", job.detail || "Ошибка выполнения дайджеста."));
    else if (job.stage === "completed") node.appendChild(el("p", "muted", job.detail || "Дайджест отправлен."));
    else if (job.detail) node.appendChild(el("p", "muted", job.detail));
  }
  function pollDigest(jobId, progress, run) {
    var attempts = 0;
    function tick() {
      attempts += 1;
      api("/api/digest/status?id=" + encodeURIComponent(jobId)).then(function (result) {
        state.digestJob = normalizeDigestResult(result); appendDigestProgress(progress, state.digestJob);
        if (state.digestJob.outcome || state.digestJob.stage === "completed" || state.digestJob.stage === "error" || attempts > 60) {
          run.disabled = false;
          if (state.digestJob.outcome === "succeeded") showToast("Дайджест отправлен.", "success");
          else if (state.digestJob.outcome === "no_posts") showToast("Нет новых постов для дайджеста.", "warning");
          else if (state.digestJob.outcome === "partial") showToast("Дайджест отправлен частично.", "warning", true);
          else if (state.digestJob.outcome) showToast(digestOutcomeText(state.digestJob), "error", true);
          else if (state.digestJob.stage === "completed") showToast("Дайджест отправлен.", "success");
          else if (state.digestJob.stage === "error") showToast(state.digestJob.detail || "Ошибка дайджеста.", "error", true);
          return;
        }
        state.digestTimer = window.setTimeout(tick, 1000);
      }).catch(function (error) {
        state.digestJob = { stage: "error", detail: apiErrorMessage(error) }; appendDigestProgress(progress, state.digestJob); run.disabled = false;
      });
    }
    tick();
  }
  function renderTab() {
    if (state.tab === "channels") return dataBody("channels", renderChannels);
    if (state.tab === "groups") return dataBody("groups", renderGroups);
    if (state.tab === "providers") return dataBody("providers", renderProviders);
    if (state.tab === "settings") return dataBody("settings", renderSettings);
    return dataBody("groups", renderDigest);
  }
  function discardSettingsAndNavigate(tab) {
    state.settingsDraft = null;
    state.tab = tab;
    refresh(tab, false);
    configureTelegramButtons();
    render();
    haptic("light");
  }
  function refresh(tab, force) {
    if (tab === "channels") return loadChannels(force);
    if (tab === "groups") return loadGroups(force);
    if (tab === "providers") return loadProviders(force);
    if (tab === "settings") return loadSettings(force);
    return Promise.all([loadGroups(force)]);
  }
  function switchTab(tab) {
    if (state.tab === "settings" && state.settingsDraft && state.data.settings && JSON.stringify(state.settingsDraft) !== JSON.stringify({
      digest_time: state.data.settings.digestTime, timezone: state.data.settings.timezone, default_model: state.data.settings.model
    })) {
      openConfirm("У вас есть несохранённые изменения.", "Выйти без сохранения?", "Выйти", function () { discardSettingsAndNavigate(tab); });
      return;
    }
    state.tab = tab; state.settingsDraft = null; refresh(tab, false); configureTelegramButtons(); render();
    haptic("light");
  }
  function render() {
    if (state.destroyed) return;
    while (app.firstChild) app.removeChild(app.firstChild);
    app.appendChild(pageShell());
    configureTelegramButtons();
  }
  function openModal(title, populate) {
    closeModal();
    var backdrop = el("div", "modal-backdrop"); attr(backdrop, "role", "dialog"); attr(backdrop, "aria-modal", "true");
    var modal = el("div", "modal");
    var header = el("div", "modal-header"); header.appendChild(el("h3", "", title));
    var close = button("×", "close-button", closeModal); attr(close, "aria-label", "Закрыть"); header.appendChild(close); modal.appendChild(header);
    var body = el("div", "modal-body"); modal.appendChild(body); backdrop.appendChild(modal); document.body.appendChild(backdrop);
    state.modal = backdrop;
    backdrop.addEventListener("click", function (event) { if (event.target === backdrop) closeModal(); });
    populate(body, closeModal);
    if (telegram && telegram.BackButton) telegram.BackButton.show();
  }
  function openConfirm(title, description, confirmLabel, onConfirm) {
    openModal("Подтверждение", function (body, close) {
      body.appendChild(el("h3", "", title)); body.appendChild(el("p", "muted", description));
      var actions = el("div", "actions");
      actions.appendChild(button("Отмена", "secondary", close));
      actions.appendChild(button(confirmLabel, "danger", function () { close(); onConfirm(); }));
      body.appendChild(actions);
    });
  }
  function closeModal() {
    if (state.modal && state.modal.parentNode) state.modal.remove();
    state.modal = null;
    configureTelegramButtons();
  }
  function configureTelegramButtons() {
    if (!telegram) return;
    if (state.modal) {
      telegram.MainButton.hide();
      telegram.BackButton.show();
      return;
    }
    telegram.BackButton.hide();
    var labels = { channels: "Добавить канал", groups: "Добавить группу", providers: "Добавить провайдера", settings: "Сохранить настройки", digest: "Запустить дайджест" };
    if (state.tab === "channels" || state.tab === "groups" || state.tab === "providers" || state.tab === "settings" || state.tab === "digest") {
      telegram.MainButton.setText(labels[state.tab]);
      telegram.MainButton.show();
      telegram.MainButton.offClick(mainButtonAction);
      telegram.MainButton.onClick(mainButtonAction);
    } else telegram.MainButton.hide();
  }
  function mainButtonAction() {
    var target = document.querySelector(
      state.tab === "channels" ? "#channel-username" :
      state.tab === "groups" ? "#group-chat-id" :
      state.tab === "providers" ? "[id='provider-name']" :
      state.tab === "settings" ? "#settings-time" :
      state.tab === "digest" ? "#digest-group" : ""
    );
    if (target) target.form ? target.form.requestSubmit() : target.focus();
    else if (state.tab === "providers") openProviderForm();
  }
  function setupTelegram() {
    if (!telegram) return false;
    if (!telegram.initData) return false;
    telegram.ready();
    telegram.expand();
    applyTheme();
    if (telegram.onEvent) telegram.onEvent("themeChanged", applyTheme);
    if (telegram.BackButton) telegram.BackButton.onClick(function () { if (state.modal) closeModal(); else telegram.close(); });
    window.addEventListener("keydown", function (event) { if (event.key === "Escape" && state.modal) closeModal(); });
    return true;
  }
  function applyTheme() {
    var scheme = telegram && telegram.colorScheme === "dark" ? "dark" : "light";
    document.documentElement.setAttribute("data-theme", scheme);
    document.documentElement.style.setProperty("--tg-theme-bg-color", pick(telegram.themeParams, ["bg_color"], scheme === "dark" ? "#17212b" : "#f4f6f8"));
    document.documentElement.style.setProperty("--tg-theme-secondary-bg-color", pick(telegram.themeParams, ["secondary_bg_color"], scheme === "dark" ? "#202b36" : "#ffffff"));
    document.documentElement.style.setProperty("--tg-theme-text-color", pick(telegram.themeParams, ["text_color"], scheme === "dark" ? "#f5f7fa" : "#17212b"));
    document.documentElement.style.setProperty("--tg-theme-hint-color", pick(telegram.themeParams, ["hint_color"], "#708090"));
    document.documentElement.style.setProperty("--tg-theme-button-color", pick(telegram.themeParams, ["button_color"], "#2aabee"));
    document.documentElement.style.setProperty("--tg-theme-button-text-color", pick(telegram.themeParams, ["button_text_color"], "#ffffff"));
  }
  function outsideTelegram() {
    while (app.firstChild) app.removeChild(app.firstChild);
    var wrap = el("div", "not-telegram"), card = el("section", "panel");
    card.appendChild(el("div", "empty-icon", "✈️"));
    card.appendChild(el("h1", "", "Откройте приложение в Telegram"));
    card.appendChild(el("p", "muted", "Это приложение должно быть открыто из Telegram. Откройте бота @tgaidigestbot и нажмите «Настройки»."));
    wrap.appendChild(card); app.appendChild(wrap);
  }
  function fatalError(message) {
    if (state.fatal || state.destroyed) return;
    state.fatal = true;
    var wrap = el("div", "not-telegram"), card = el("section", "panel");
    card.appendChild(el("div", "empty-icon", "⚠️"));
    card.appendChild(el("h1", "", "Что-то пошло не так"));
    card.appendChild(el("p", "muted", message || "Не удалось загрузить приложение. Попробуйте перезагрузить страницу."));
    card.appendChild(button("Перезагрузить", "primary", function () { window.location.reload(); }));
    wrap.appendChild(card);
    while (app.firstChild) app.removeChild(app.firstChild);
    app.appendChild(wrap);
  }
  function start() {
    if (!setupTelegram()) { outsideTelegram(); return; }
    render();
    refresh(state.tab, false);
  }
  window.addEventListener("error", function () {
    fatalError("Произошла ошибка интерфейса. Попробуйте перезагрузить приложение.");
  });
  window.addEventListener("unhandledrejection", function () {
    fatalError("Не удалось выполнить операцию. Попробуйте перезагрузить приложение.");
  });
  window.addEventListener("beforeunload", function () {
    state.destroyed = true;
    if (state.digestTimer) window.clearTimeout(state.digestTimer);
    if (state.rateLimitTimer) window.clearInterval(state.rateLimitTimer);
    if (telegram && telegram.MainButton) telegram.MainButton.offClick(mainButtonAction);
  });
  start();
}());
