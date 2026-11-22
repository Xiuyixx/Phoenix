(function () {
  "use strict";

  var API = "/v0/management/phoenix";
  var CONFIG_API = "/v0/management/plugins/phoenix/config";
  var KEY_STORE = "phoenix-mgmt-key";

  function $(id) { return document.getElementById(id); }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (name) {
        var value = attrs[name];
        if (value === undefined || value === null || value === false) return;
        if (name === "text") node.textContent = value;
        else if (name === "class") node.className = value;
        else if (name === "style") node.setAttribute("style", value);
        else if (name.indexOf("on") === 0) node.addEventListener(name.slice(2), value);
        else node.setAttribute(name, value === true ? "" : value);
      });
    }
    (children || []).forEach(function (child) {
      if (child === null || child === undefined) return;
      node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
    });
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // ---- Management key (kept exactly as in v0.0.7) ----
  // The Management Center stores the key in localStorage ("cli-proxy-auth"),
  // XOR-obfuscated with the host (v2) or host and user agent (v1). Fall back
  // to asking the user for the key, kept only in this tab's sessionStorage.
  function centerKey() {
    try {
      var raw = localStorage.getItem("cli-proxy-auth");
      if (!raw) return "";
      var legacy = raw.indexOf("enc::v1::") === 0;
      var prefix = legacy ? "enc::v1::" : "enc::v2::";
      if (raw.indexOf(prefix) === 0) {
        var data = atob(raw.slice(prefix.length));
        var mask = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|" + location.host + (legacy ? "|" + navigator.userAgent : ""));
        var bytes = new Uint8Array(data.length);
        for (var i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) ^ mask[i % mask.length];
        raw = new TextDecoder().decode(bytes);
      }
      var parsed = JSON.parse(raw);
      var stored = parsed && parsed.state ? parsed.state : parsed;
      return stored && typeof stored.managementKey === "string" ? stored.managementKey : "";
    } catch (e) { return ""; }
  }

  var key = "";
  try { key = sessionStorage.getItem(KEY_STORE) || centerKey(); } catch (e) { key = ""; }

  function askForKey(message) {
    key = "";
    try { sessionStorage.removeItem(KEY_STORE); } catch (e) {}
    $("key-section").hidden = false;
    if (message) $("key-error").textContent = message;
  }

  // ---- API ----
  // timeout, in milliseconds, aborts a request that takes longer.
  function api(method, path, body, timeout) {
    var opts = { method: method, headers: { "Authorization": "Bearer " + key, "Accept": "application/json" } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    var timer = null;
    if (timeout && window.AbortController) {
      var abort = new AbortController();
      opts.signal = abort.signal;
      timer = setTimeout(function () { abort.abort(); }, timeout);
    }
    var done = function () { clearTimeout(timer); };
    return fetch(path, opts).then(function (res) {
      return res.json().catch(function (err) {
        if (opts.signal && opts.signal.aborted) throw err;
        return {};
      }).then(function (data) {
        if (res.status === 401) {
          askForKey("管理密钥无效或已过期，请重新输入。");
          var unauth = new Error("unauthorized");
          unauth.status = 401;
          throw unauth;
        }
        if (!res.ok) {
          var err = new Error(data.error || data.message || ("请求失败：" + res.status));
          err.status = res.status;
          throw err;
        }
        return data;
      });
    }).then(function (data) { done(); return data; }, function (err) { done(); throw err; });
  }

  var toastTimer = null;
  function toast(message) {
    var node = $("toast");
    clear(node);
    node.appendChild(document.createTextNode(message));
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { node.hidden = true; }, 5000);
  }

  // ---- Time (all shown in the plugin's time zone) ----
  function offsetSeconds() {
    var value = status ? Number(status.utc_offset_seconds) : NaN;
    return isFinite(value) ? value : -new Date().getTimezoneOffset() * 60;
  }

  function parseTime(value) {
    if (!value) return null;
    var t = new Date(value);
    if (isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return null;
    return t;
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }

  function shifted(t) { return new Date(t.getTime() + offsetSeconds() * 1000); }

  var WEEKDAYS = "周日,周一,周二,周三,周四,周五,周六".split(",");

  function fmtDateTime(t) {
    if (!t) return "—";
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate()) + " " + pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  // A time ahead with its weekday, for resets and the next ignition.
  function fmtDateTimeDay(t) {
    if (!t) return "—";
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate()) + " " + WEEKDAYS[s.getUTCDay()] + " " +
      pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtClock(t) {
    var s = shifted(t);
    return pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtDay(t) {
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate());
  }

  function fmtDuration(ms) {
    if (ms <= 0) return "已到";
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + " 分钟";
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return hours + " 小时 " + (minutes % 60) + " 分";
    return Math.round(hours / 24) + " 天";
  }

  function fmtIn(t) {
    if (!t) return "";
    var ms = t.getTime() - Date.now();
    if (ms < 60000) return "即将";
    return fmtDuration(ms) + "后";
  }

  function fmtAgo(t) {
    if (!t) return "";
    var ms = Date.now() - t.getTime();
    if (ms < 60000) return "刚刚";
    return fmtDuration(ms) + "前";
  }

  function fmtCountdown(ms) {
    if (ms <= 0) return "已重置";
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + "m";
    var hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + "h " + pad(minutes % 60) + "m";
    return Math.floor(hours / 24) + "d " + (hours % 24) + "h";
  }

  function pct(v) { return v == null ? "—" : Math.round(v) + "%"; }

  function fmtDiff(d) { return d == null ? "—" : (d > 0 ? "+" : d < 0 ? "−" : "") + Math.abs(d) + "%"; }

  // Bars and the chart color quota in five levels: empty at 0% and full at
  // 100%, judged on the raw value; between them low, medium and high, split
  // by the first two notification thresholds.
  function quotaThresholds() {
    var cfg = status && status.config ? status.config : {};
    var th = cfg.alerts && Array.isArray(cfg.alerts.thresholds) ? cfg.alerts.thresholds : [];
    var notice = Number(th[0]), low = Number(th[1]);
    return { notice: isFinite(notice) ? notice : 50, low: isFinite(low) ? low : 20 };
  }

  var LEVEL_EPSILON = 1e-6;

  function quotaLevel(remaining) {
    var t = quotaThresholds();
    if (remaining <= LEVEL_EPSILON) return "empty";
    if (remaining >= 100 - LEVEL_EPSILON) return "full";
    if (remaining <= t.low) return "low";
    if (remaining <= t.notice) return "medium";
    return "high";
  }

  function badge(kind, text) {
    return el("span", { class: "badge badge-" + kind }, [el("span", { class: "badge-dot" }), document.createTextNode(text)]);
  }

  // ---- State ----
  var status = null;
  var settingsFilled = false;
  var refreshTimer = null;

  // ---- Header + notices ----
  function renderHeader() {
    var state = $("state");
    clear(state);
    if (!status.enabled) state.appendChild(badge("muted", "已停用"));
    else if (!status.running) state.appendChild(badge("warning", "启动中"));
    else state.appendChild(badge("success", "运行中"));
    var parts = [];
    var last = parseTime(status.last_poll), next = parseTime(status.next_poll);
    if (last) parts.push("上次查询 " + fmtClock(last) + "（" + fmtAgo(last) + "）");
    if (next) parts.push("下次 " + fmtClock(next) + "（" + fmtIn(next) + "）");
    var meta = $("meta");
    clear(meta);
    meta.appendChild(document.createTextNode("v" + (status.version || "")));
    if (parts.length) meta.appendChild(document.createTextNode(" · " + parts.join(" · ")));
  }

  function notice(text) {
    return el("div", { class: "notice" }, [el("span", { class: "notice-icon", text: "!" }), el("span", { text: text })]);
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    var errors = (status && status.errors) || {};
    if (errors.config_error) box.appendChild(notice("配置无法解析，仍在使用上一份配置：" + errors.config_error));
    if (errors.list_error) box.appendChild(notice(errors.list_error));
    if (errors.models_error) box.appendChild(notice("模型列表不可用：" + errors.models_error));
    if (errors.notify_error) box.appendChild(notice("通知设置无效，未启用：" + errors.notify_error));
  }

  // ---- Quota cards ----
  // An empty window shows no fill: its whole track turns dark red and
  // striped instead.
  function quotaRow(w, index) {
    var remaining = Math.max(0, Math.min(100, w.remaining));
    var level = quotaLevel(w.remaining);
    var reset = parseTime(w.reset);
    var meta = [el("span", { class: "quota-percent" + (level === "empty" ? " percent-empty" : ""), text: Math.round(remaining) + "%" })];
    if (reset) {
      var left = reset.getTime() - Date.now();
      meta.push(el("span", { class: "quota-reset", text: fmtDateTimeDay(reset) }));
      meta.push(el("span", { class: "quota-countdown" + (left > 0 && left <= 3600000 ? " soon" : ""), text: fmtCountdown(left) }));
    }
    return el("div", { class: "quota-row" }, [
      el("div", {}, [
        el("span", { class: "quota-name", text: w.label || (w.short === "5h" ? "5 小时窗口" : w.short === "7d" ? "7 天窗口" : w.id || "") }),
        el("span", { class: "quota-meta" }, meta)
      ]),
      el("div", { class: "bar" + (level === "empty" ? " bar-empty" : ""), role: "meter", "aria-valuemin": "0", "aria-valuemax": "100",
        "aria-valuenow": String(Math.round(remaining)), "aria-label": (w.label || "") + " 剩余" },
        level === "empty" ? [] : [el("div", { class: "fill fill-" + level, style: "width:" + remaining + "%" })])
    ]);
  }

  function freshness(windows) {
    var latest = null, source = "";
    (windows || []).forEach(function (w) {
      var t = parseTime(w.observed_at);
      if (t && (!latest || t > latest)) { latest = t; source = w.source; }
    });
    if (!latest) return null;
    return el("div", { class: "group-foot" }, [
      el("span", { class: "source source-" + source, text: source === "passive" ? "被动" : "主动" }),
      el("span", { text: "更新于 " + fmtAgo(latest) + "前" })
    ]);
  }

  var WINDOW_ORDER = { "5h": 0, "7d": 1 };

  function sortedWindows(account) {
    return (account.windows || []).slice().sort(function (a, b) {
      var ra = WINDOW_ORDER[a.kind || a.short] !== undefined ? WINDOW_ORDER[a.kind || a.short] : 2;
      var rb = WINDOW_ORDER[b.kind || b.short] !== undefined ? WINDOW_ORDER[b.kind || b.short] : 2;
      return ra - rb;
    });
  }

  function accountTitle(account) {
    return account.title || account.auth_id || ("账号 " + (account.auth_index != null ? account.auth_index : ""));
  }

  function renderAccounts() {
    var box = $("accounts");
    clear(box);
    var accounts = (status && status.accounts) || [];
    if (!accounts.length) {
      box.appendChild(el("p", { class: "empty", text: status.running ? "暂无额度数据。插件启动后会从 CPA 的使用记录和主动查询中填充。" : "CPA 启动约 20 秒后开始第一次查询。" }));
      return;
    }
    accounts.forEach(function (account) {
      var card = el("div", { class: "account" });
      var head = el("div", { class: "account-head" }, [
        el("span", { class: "account-name", text: accountTitle(account) })
      ]);
      if (account.provider) head.appendChild(el("span", { class: "badge badge-muted", text: account.provider }));
      card.appendChild(head);
      if (account.auth_id && account.auth_id !== account.title) {
        card.appendChild(el("div", { class: "account-id", text: account.auth_id }));
      }
      sortedWindows(account).forEach(function (w, i) { card.appendChild(quotaRow(w, i)); });
      var foot = freshness(account.windows);
      if (foot) card.appendChild(foot);
      var cooldown = parseTime(account.cooldown_until);
      if (cooldown && account.stale_cooldown) {
        card.appendChild(el("div", { class: "error-text", text: "额度已恢复，但 CPA 仍冷却到 " + fmtDateTime(cooldown) + "，期间的请求和点火都会被拒绝；在 CPA 管理中心清除这个账号的冷却。" }));
      } else if (cooldown) {
        card.appendChild(el("div", { class: "cell-note", text: "CPA 冷却到 " + fmtDateTime(cooldown) + "（" + fmtIn(cooldown) + "）" }));
      }
      var problem = account.error || account.credential_problem || "";
      if (problem) card.appendChild(el("div", { class: "error-text", text: problem }));
      var refresh = el("button", { type: "button", class: "button secondary sm", text: "刷新", onclick: function () {
        runAction(refresh, API + "/refresh", { auth_index: account.auth_index }, "已刷新 " + accountTitle(account));
      } });
      card.appendChild(el("div", { class: "account-head cell-note" }, [
        el("span", { class: "cell-note", text: account.plan ? "套餐：" + account.plan : "" }),
        refresh
      ]));
      box.appendChild(card);
    });
  }

  // ---- Ignition targets ----
  function targetStatus(t) {
    if (t.status === "circuit") {
      var until = parseTime(t.circuit_until);
      return { kind: "failure", text: "熔断", title: until ? "熔断到 " + fmtDateTime(until) : (t.last_error || "") };
    }
    if (t.status === "cooling") return { kind: "warning", text: "冷却中", title: t.last_error || "" };
    if (t.status === "paused") return { kind: "muted", text: "暂停", title: t.last_error || "" };
    if (t.consecutive_failures > 0) return { kind: "warning", text: "失败 " + t.consecutive_failures + " 次", title: t.last_error || "" };
    return { kind: "success", text: "正常" };
  }

  function renderTargets() {
    var body = $("targets");
    clear(body);
    var ignition = (status.config && status.config.ignition) || {};
    var endMinutes = (Number(ignition.end_hour) || 0) * 60 + (Number(ignition.end_grace_minutes) || 0);
    $("ignition-note").textContent = "每天 " + pad(Number(ignition.start_hour) || 0) + ":00 开始，之后在每次重置后 " +
      (Number(ignition.grace_seconds) || 0) + " 秒点火，最晚到 " + pad(Math.floor(endMinutes / 60) % 24) + ":" + pad(endMinutes % 60) + "。";
    var targets = (status && status.targets) || [];
    if (!targets.length) {
      body.appendChild(el("tr", {}, [el("td", { colspan: "6", class: "empty", text: "没有启用点火的目标。" })]));
      return;
    }
    targets.forEach(function (t) {
      var st = targetStatus(t);
      var rowStatus = el("div", { class: "row-status" });
      var resetBtn = el("button", { type: "button", class: "button secondary sm", text: "立即重置", onclick: function () {
        resetBtn.disabled = true; igniteBtn.disabled = true;
        rowStatus.className = "row-status";
        rowStatus.textContent = "正在提交重置请求…";
        api("POST", API + "/reset", { auth_index: t.auth_index }).then(function (data) {
          if (data.status) { status = data.status; renderStatus(); }
          rowStatus.textContent = data.ok === false ? "重置失败：" + (data.error || "") : "重置请求已提交";
          if (data.ok === false) rowStatus.classList.add("error");
          loadHistory(true);
        }).catch(function (err) {
          if (err.message !== "unauthorized") { rowStatus.textContent = "重置失败：" + err.message; rowStatus.classList.add("error"); }
        }).then(function () { resetBtn.disabled = false; igniteBtn.disabled = false; });
      } });
      var igniteBtn = el("button", { type: "button", class: "button primary sm", text: "立即点火", onclick: function () {
        if (!confirm("立即向 " + (t.label || accountTitle({ auth_index: t.auth_index })) + " 发送一次点火请求？这会开始一个新的 5 小时窗口。")) return;
        resetBtn.disabled = true; igniteBtn.disabled = true;
        rowStatus.className = "row-status";
        rowStatus.textContent = "正在提交点火请求…";
        api("POST", API + "/ignite", { auth_index: t.auth_index }).then(function (data) {
          if (data.status) { status = data.status; renderStatus(); }
          rowStatus.textContent = data.ok === false ? "点火失败：" + (data.error || "") : "点火请求已提交";
          if (data.ok === false) rowStatus.classList.add("error");
          loadHistory(true);
        }).catch(function (err) {
          if (err.message !== "unauthorized") { rowStatus.textContent = "点火失败：" + err.message; rowStatus.classList.add("error"); }
        }).then(function () { resetBtn.disabled = false; igniteBtn.disabled = false; });
      } });
      var reset = parseTime(t.reset), last = parseTime(t.last_success);
      var statusCell = el("td", {}, [badge(st.kind, st.text)]);
      if (st.title) statusCell.appendChild(el("div", { class: "cell-note", text: st.title }));
      body.appendChild(el("tr", {}, [
        el("td", { class: "strong", text: t.label || ("账号 " + t.auth_index) }),
        el("td", { class: "mono", text: reset ? fmtDateTimeDay(reset) : "—" }),
        el("td", { class: "mono", text: last ? fmtDateTime(last) : "—" }),
        el("td", { class: "mono", text: t.last_model || "—" }),
        statusCell,
        el("td", { class: "actions" }, [el("div", { class: "row-actions" }, [resetBtn, igniteBtn]), rowStatus])
      ]));
    });
  }

  // ---- Events ----
  var EVENT_STYLE = {
    ignite: ["success", "点火成功"], ignite_manual: ["success", "手动点火"], ignite_failed: ["warning", "点火失败"],
    ignite_paused: ["failure", "点火暂停"], model_refused: ["warning", "模型被拒"], cooldown_stale: ["warning", "冷却未解除"],
    plan_change: ["warning", "套餐变化"], notify: ["muted", "已推送"], notify_failed: ["warning", "推送失败"],
    codex_reset: ["muted", "重置信号"], error: ["failure", "错误"], reset: ["success", "额度重置"],
    exhausted: ["warning", "额度耗尽"], credential_invalid: ["failure", "凭证失效"],
    credential_suspended: ["failure", "账号停用"], credential_restored: ["success", "凭证恢复"],
    ok: ["success", "完成"], info: ["muted", "事件"]
  };
  var EVENTS_COLLAPSED = 12, EVENTS_MAX = 100;
  var eventsExpanded = false;
  var EVENTS_STORE = "phoenix-events-5h";
  var showFiveHourResets = false;
  try { showFiveHourResets = localStorage.getItem(EVENTS_STORE) === "1"; } catch (err) {}

  function isFiveHourReset(ev) {
    var p = ev.params || {};
    var win = String(p.window || ev.window || "");
    if (win === "5h" || win === "five-hour") return ev.kind === "reset" || /重置/.test(ev.message || "");
    if (ev.kind !== "reset") return false;
    return /5\s*(h|小时)|five-hour/i.test((ev.message || "") + " " + (ev.kind || ""));
  }

  function eventLabel(ev) {
    if (ev.label) return ev.label;
    if (ev.auth_index != null) {
      var hit = null;
      (status.accounts || []).forEach(function (a) { if (String(a.auth_index) === String(ev.auth_index)) hit = a; });
      if (hit) return accountTitle(hit);
    }
    return "";
  }

  function renderEvents() {
    var list = $("events");
    clear(list);
    var more = $("events-more");
    var filter = $("events-5h"), filterText = $("events-5h-label");
    var events = ((status && status.events) || []).slice().sort(function (a, b) {
      var ta = parseTime(a.time), tb = parseTime(b.time);
      return (tb ? tb.getTime() : 0) - (ta ? ta.getTime() : 0);
    });
    var fiveHour = 0, hidden = 0;
    var rows = events.filter(function (ev) {
      if (isFiveHourReset(ev)) {
        fiveHour++;
        if (!showFiveHourResets) { hidden++; return false; }
      }
      return true;
    });
    if (filter) {
      filter.checked = showFiveHourResets;
      filterText.textContent = fiveHour ? "显示 5 小时重置（" + fiveHour + "）" : "显示 5 小时重置";
    }
    if (!rows.length) {
      list.appendChild(el("p", { class: "empty", text: "暂无事件" }));
      more.hidden = true;
      return;
    }
    var total = Math.min(rows.length, EVENTS_MAX);
    rows.slice(0, eventsExpanded ? EVENTS_MAX : EVENTS_COLLAPSED).forEach(function (ev) {
      var known = EVENT_STYLE[ev.kind];
      var style = known ? known[0] : (ev.kind === "error" ? "failure" : "muted");
      var name = known ? known[1] : (ev.kind || "事件");
      var detail = ev.message || ev.detail || "";
      var label = eventLabel(ev);
      var cell = [el("span", { class: "event-text", title: detail || null, text: detail })];
      if (ev.notify === "sent") cell.push(el("span", { class: "event-mark", text: "已推送" }));
      else if (ev.notify === "failed") cell.push(el("span", { class: "event-mark failed", text: "推送失败" }));
      list.appendChild(el("div", { class: "event-row", role: "row" }, [
        el("span", { class: "event-time", role: "cell", text: fmtDateTime(parseTime(ev.time)) }),
        el("span", { class: "event-type", role: "cell" }, [badge(style, name)]),
        el("span", { class: "event-label", role: "cell", text: label || "—" }),
        el("span", { class: "event-detail", role: "cell" }, cell)
      ]));
    });
    more.hidden = total <= EVENTS_COLLAPSED;
    more.textContent = eventsExpanded ? "收起" : "显示更多（共 " + total + " 条）";
  }

  function renderStatus() {
    if (!status) return;
    checkApplied();
    renderHeader();
    renderNotices();
    if (renderWindowButtons() && (history || historyLoading)) loadHistory();
    renderAccounts();
    renderTargets();
    renderEvents();
    if (!settingsFilled) fillSettings();
  }

  function loadStatus(timeout) {
    return api("GET", API + "/status", undefined, timeout).then(function (data) {
      status = data;
      renderStatus();
    });
  }

  function runAction(button, path, body, success) {
    button.disabled = true;
    return api("POST", path, body).then(function (data) {
      if (data.status) { status = data.status; renderStatus(); }
      toast(data.ok === false ? "失败：" + (data.error || "") : success);
      loadHistory(true);
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast("失败：" + err.message);
    }).then(function () { button.disabled = false; });
  }

  // ---- Chart ----
  // Time ranges follow the quota window, in hours for the 5-hour quota and
  // days for the 7-day quota: one unit; half of a window plus one (3h, 4d);
  // a window plus one (6h, 8d); half a day or month (12h, 15d); a day or a
  // month; five windows plus one (26h, 36d).
  var RANGES = {
    "5h": [["1h", 1, "h"], ["3h", 3, "h"], ["6h", 6, "h"], ["12h", 12, "h"], ["24h", 24, "h"], ["26h", 26, "h"]],
    "7d": [["24h", 1, "d"], ["4d", 4, "d"], ["8d", 8, "d"], ["15d", 15, "d"], ["1mo", 1, "mo"], ["36d", 36, "d"]]
  };
  var DEFAULT_RANGE = { "5h": "6h", "7d": "8d" };
  function extendsAhead(kind, name) {
    return (RANGES[kind] || []).some(function (r, i) { return i >= 2 && r[0] === name; });
  }
  var LANE = 22, LANE_GAP = 6, AXIS_H = 22, EVENT_H = 24;
  // A rise of this many points between two samples is a reset.
  var RESET_JUMP = 5;
  // Quotas report whole percents; steps this small are a steady decline.
  var QUANTUM = 2;
  // A passive value unchanged for longer than this is held until now when the
  // rate of use is estimated, so a pause shows as one.
  var HOLD_AFTER = 120;
  var CHAR_W = 6.6, LABEL_H = 16;

  var chartWindow = "5h";
  var range = "6h";
  var chartSort = "default";
  var chartExpanded = {};
  var chartSelected = null;
  var history = null;
  var historyWindow = "";
  var historyRange = "";
  var chartView = null;
  var hoverSource = "";
  var hoverListeners = [];
  var snapPoints = [];

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  var LEVEL_VARS = { empty: "--quota-empty-color", low: "--danger", medium: "--quota-medium-color", high: "--ok", full: "--quota-full-color" };

  function levelRect(root, attrs, level) {
    var fill = {};
    Object.keys(attrs).forEach(function (name) { fill[name] = attrs[name]; });
    fill.fill = cssVar(LEVEL_VARS[level]);
    root.appendChild(svg("rect", fill));
    if (level !== "empty") return;
    var stripes = {};
    Object.keys(attrs).forEach(function (name) { if (name !== "fill-opacity") stripes[name] = attrs[name]; });
    stripes.fill = "url(#chart-hatch-empty)";
    root.appendChild(svg("rect", stripes));
  }

  function unixDate(t) { return new Date(t * 1000); }

  // The chart offers the window kinds the accounts report, and those whose
  // loaded history still has a sample in the range of an inactive window;
  // both while none are known.
  function windowKinds() {
    var seen = {};
    (status.accounts || []).forEach(function (a) {
      (a.windows || []).forEach(function (w) { if (w.kind) seen[w.kind] = true; });
    });
    ["5h", "7d"].forEach(function (kind) {
      var cached = historyCache[historyKey(kind, kind === chartWindow ? range : rangeFor(kind))];
      if (!seen[kind] && cached && recentSamples(cached.data, kind)) seen[kind] = true;
    });
    var kinds = ["5h", "7d"].filter(function (k) { return seen[k]; });
    return kinds.length ? kinds : ["5h", "7d"];
  }

  function recentSamples(data, kind) {
    var from = new Date(data.from).getTime() / 1000;
    return (data.series || []).some(function (s) {
      return (s.window || s.kind) === kind && s.points && s.points.length && s.points[s.points.length - 1][0] >= from;
    });
  }

  // Shows the window buttons of the reported kinds. When the chosen kind is
  // not among them, it switches to the first one and returns true.
  function renderWindowButtons() {
    var kinds = windowKinds();
    Array.prototype.forEach.call($("window-buttons").querySelectorAll("button"), function (b) {
      b.hidden = kinds.indexOf(b.getAttribute("data-value")) < 0;
    });
    if (kinds.indexOf(chartWindow) >= 0) return false;
    chartWindow = kinds[0];
    range = rangeFor(chartWindow);
    pressSegment("window-buttons", chartWindow);
    renderRangeButtons();
    return true;
  }

  function valueAt(points, t) {
    var value = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] <= t) value = points[i];
      else break;
    }
    return value;
  }

  function gapLimit() {
    return sampleStep() * 3.5;
  }

  function sampleStep() {
    var cfg = status && status.config ? status.config : {};
    return Math.max(history.bucket_seconds || 0, Number(cfg.poll_interval_seconds) || 300);
  }

  // Pooled remaining of several accounts: the mean percentage (equal weights;
  // Phoenix has no per-account weights), each point also carrying the lowest
  // and highest account for the range band, and 1 when an account's share is
  // estimated (gapValue). key is "points" or "floor".
  function pooled(rows, limit, key) {
    var times = [];
    rows.forEach(function (r) { r[key].forEach(function (p) { times.push(p[0]); }); });
    times.sort(function (a, b) { return a - b; });
    var out = [];
    times.forEach(function (t, i) {
      if (i && t === times[i - 1]) return;
      var sum = 0, total = 0, lo = 101, hi = -1, estimated = 0, fresh = false;
      rows.forEach(function (r) {
        var v = gapValue(r[key], r.resets, t, limit);
        if (!v) return;
        if (v.estimated) estimated = 1;
        else fresh = true;
        sum += v.value; total += 1; lo = Math.min(lo, v.value); hi = Math.max(hi, v.value);
      });
      if (fresh) out.push([t, sum / total, 0, lo, hi, estimated]);
    });
    return out;
  }

  // The reset an account's window was due at as of time t, from the resets
  // of its history ([sample time, reset due]); 0 when unknown.
  function resetAsOf(resets, t) {
    var at = 0;
    (resets || []).forEach(function (r) { if (r[0] <= t) at = r[1]; });
    return at;
  }

  // The value of one account at time t for a pooled total: its last sample
  // when recent, otherwise an estimate { value, estimated: true }; null
  // before its first sample.
  function gapValue(points, resets, t, limit) {
    var p = valueAt(points, t);
    if (!p) return null;
    if (t - p[0] <= limit) return { value: p[1], estimated: false };
    var next = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] > t) { next = points[i]; break; }
    }
    var reset = resetAsOf(resets, p[0]);
    var resetIn = reset > p[0] && (!next || reset <= next[0]);
    var value;
    if (next && !resetIn && next[1] - p[1] < RESET_JUMP) {
      value = Math.min(p[1], p[1] + (next[1] - p[1]) * (t - p[0]) / (next[0] - p[0]));
    } else if (resetIn && t >= reset) {
      value = next ? 100 + (next[1] - 100) * (t - reset) / Math.max(1, next[0] - reset) : 100;
    } else {
      value = p[1];
    }
    return { value: value, estimated: true };
  }

  // The lowest remaining so far in each cycle. A rise by a reset starts a
  // new cycle.
  function cycleFloor(points) {
    var low = null;
    return points.map(function (p) {
      low = low == null || p[1] - low >= RESET_JUMP ? p[1] : Math.min(low, p[1]);
      var q = p.slice();
      q[1] = low;
      return q;
    });
  }

  // One row per account. With several accounts a pooled "合计" row leads and
  // expands into one row per account. floor holds the samples as cycleFloor()
  // leaves them, for the band and the rate of use.
  //
  // A window shows while the status still reports it, or while it has a
  // sample in the chosen range. A window that only has samples is inactive:
  // it gets a row of its own after the pooled row, is left out of the total
  // and of "lowest first", and shows no projection or even pace.
  function chartRows() {
    var info = {}, accountOf = {}, current = [];
    (status.accounts || []).forEach(function (a, index) {
      accountOf["auth|" + a.auth_index] = index;
      (a.windows || []).forEach(function (w) {
        var reset = parseTime(w.reset);
        info[a.auth_index + "|" + w.id] = { reset: reset ? reset.getTime() / 1000 : 0, remaining: w.remaining };
        if ((w.kind || w.short) === historyWindow) {
          current.push({ id: "auth|" + a.auth_index + "|" + w.id, name: accountTitle(a), sub: a.auth_id || String(a.auth_index),
            group: a.auth_index, window: w.id, kind: w.kind || w.short, points: [], floor: [], resets: [], resetTimes: [],
            reset: reset ? reset.getTime() / 1000 : null, last: w.remaining, account: index, inactive: false, weight: 1 });
        }
      });
    });
    var limit = gapLimit();
    var from = new Date(history.from).getTime() / 1000;
    var seen = {};
    var series = (history.series || []).filter(function (s) { return s.points && s.points.length; });
    series.forEach(function (s) { seen[s.auth_index + "|" + s.window] = true; });
    var rows = [];
    series.forEach(function (s, seq) {
      if ((s.window || s.kind) !== historyWindow) return;
      var meta = info[s.auth_index + "|" + s.window];
      var lastPoint = s.points[s.points.length - 1];
      if (!meta && lastPoint[0] < from) return;
      var account = accountOf["auth|" + s.auth_index];
      var title = "";
      if (account !== undefined) title = accountTitle(status.accounts[account]);
      rows.push({ id: "auth|" + s.auth_index + "|" + s.window, name: s.label || title || ("账号 " + s.auth_index),
        sub: (account !== undefined ? (status.accounts[account].auth_id || "") : "") || String(s.auth_index || ""),
        group: s.auth_index, window: s.window, kind: s.window || s.kind, points: s.points, floor: cycleFloor(s.points),
        resets: Array.isArray(s.resets) ? s.resets : [], resetTimes: Array.isArray(s.resetTimes) ? s.resetTimes : [],
        reset: meta ? meta.reset : null, last: lastPoint ? lastPoint[1] : (meta ? meta.remaining : 0),
        lastAt: lastPoint ? lastPoint[0] : null, account: account === undefined ? status.accounts.length + seq : account,
        inactive: !meta, weight: 1, orphan: account === undefined });
    });
    current.forEach(function (c) {
      if (!seen[c.group + "|" + c.window]) rows.push(c);
    });
    var live = rows.filter(function (r) { return !r.inactive; });
    var gone = rows.filter(function (r) { return r.inactive; });
    var lowFirst = chartSort === "low";
    var byAccount = function (a, b) { return (a.account - b.account); };
    var byLast = function (a, b) { return (a.last - b.last) || byAccount(a, b); };
    live.sort(lowFirst ? byLast : byAccount);
    gone.sort(byAccount);
    var out = [];
    if (live.length > 1) out.push(pooledRow(live, limit));
    else if (live.length === 1) out.push(live[0]);
    out.push.apply(out, gone);
    return out;
  }

  // The total of the active accounts.
  function pooledRow(kids, limit) {
    var now = Date.now() / 1000;
    var resets = kids.map(function (k) { return k.reset; }).filter(function (t) { return t > now; });
    return { id: "group:all", name: "合计", children: kids, points: pooled(kids, limit, "points"), floor: pooled(kids, limit, "floor"),
      last: kids.reduce(function (sum, k) { return sum + k.last; }, 0) / kids.length,
      reset: resets.length ? Math.min.apply(null, resets) : null, groups: kids.map(function (k) { return k.group; }) };
  }

  function findRow(rows, id) {
    var hit = null;
    rows.forEach(function (r) {
      if (r.id === id) hit = r;
      (r.children || []).forEach(function (c) { if (c.id === id) hit = c; });
    });
    return hit;
  }

  // The right end of the time axis: for a range that extends ahead, the
  // reset of the selected row's window (the earliest account's for a pooled
  // row), otherwise now.
  function axisEnd(row) {
    var from = new Date(history.from).getTime() / 1000, now = new Date(history.to).getTime() / 1000;
    if (!extendsAhead(historyWindow, historyRange) || !row || !row.reset || row.reset <= now || row.reset - now >= now - from) return now;
    return row.reset;
  }

  // The axis spans the chosen range and ends at end.
  function geometry(width, end) {
    var now = new Date(history.to).getTime() / 1000;
    var to = end || now, from = to - (now - new Date(history.from).getTime() / 1000);
    var labelW = width < 640 ? 120 : 190, rightW = width < 640 ? 112 : 136;
    var plotW = width - labelW - rightW;
    return { from: from, to: to, now: now, width: width, labelW: labelW, rightW: rightW, plotW: plotW,
      x: function (t) { return labelW + (t - from) / (to - from) * plotW; },
      t: function (px) { return from + (px - labelW) / plotW * (to - from); } };
  }

  function chartTicks(g) {
    var span = g.to - g.from;
    var step = span <= 3600 ? 600 : span <= 3 * 3600 ? 1800 : span <= 6 * 3600 ? 3600 : span <= 12 * 3600 ? 2 * 3600 : span <= 2 * 86400 ? 4 * 3600 : span <= 8 * 86400 ? 86400 : span <= 15 * 86400 ? 2 * 86400 : 5 * 86400;
    var offset = offsetSeconds(), list = [];
    for (var t = Math.ceil((g.from + offset) / step) * step - offset; t <= g.to; t += step) list.push(t);
    return { list: list, label: function (t) { return span <= 2 * 86400 ? fmtClock(unixDate(t)) : fmtDay(unixDate(t)); } };
  }

  // Contiguous stretches of samples; each ends at its last sample plus one
  // gap limit, or at "now" when that is sooner.
  function runs(points, limit, end) {
    var out = [], cur = [];
    points.forEach(function (p) {
      if (cur.length && p[0] - cur[cur.length - 1][0] > limit) { out.push(cur); cur = []; }
      cur.push(p);
    });
    if (cur.length) out.push(cur);
    return out.map(function (r, i) {
      var last = r[r.length - 1][0];
      var stop = i === out.length - 1 ? Math.min(end, last + limit) : last + Math.min(limit, 300);
      return { pts: r, start: r[0][0], end: Math.max(stop, last) };
    });
  }

  function hatchPattern(root) {
    var defs = svg("defs", {});
    var pattern = svg("pattern", { id: "chart-hatch", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
    pattern.appendChild(svg("rect", { width: 2, height: 6, fill: cssVar("--chart-hatch") }));
    defs.appendChild(pattern);
    var empty = svg("pattern", { id: "chart-hatch-empty", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
    empty.appendChild(svg("rect", { width: 2.5, height: 6, fill: cssVar("--quota-empty-stripe") }));
    defs.appendChild(empty);
    root.appendChild(defs);
    return defs;
  }

  var SVG_NS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs) {
    var node = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (name) { node.setAttribute(name, attrs[name]); });
    return node;
  }

  function svgText(attrs, text) {
    var node = svg("text", attrs);
    node.textContent = text;
    return node;
  }

  // Hatching from a to b, cut to the axis: missing data, and the time after
  // now, which has none yet.
  function hatchRect(root, g, a, b, y0, height) {
    a = Math.max(a, g.from);
    if (g.x(b) - g.x(a) > 1) root.appendChild(svg("rect", { x: g.x(a), y: y0, width: g.x(b) - g.x(a), height: height, fill: "url(#chart-hatch)" }));
  }

  function dataEnd(g, cursor, limit) { return cursor < g.now - limit ? cursor : g.now; }

  // A band of solid color per quota level. Samples merge while the level
  // stays the same, and edges snap to whole pixels so neighbours leave no seam.
  function drawBand(root, g, points, y0, limit, inactive) {
    var cursor = g.from;
    runs(points, limit, inactive ? points[points.length - 1][0] : g.now).forEach(function (run) {
      if (run.start > cursor + limit) hatchRect(root, g, cursor, run.start, y0, LANE);
      cursor = run.end;
      var segments = [];
      run.pts.forEach(function (p, i) {
        var end = i + 1 < run.pts.length ? run.pts[i + 1][0] : run.end;
        var level = quotaLevel(p[1]), last = segments[segments.length - 1];
        if (last && last.level === level) last.end = end;
        else segments.push({ start: Math.max(g.from, p[0]), end: end, level: level });
      });
      segments.forEach(function (seg) {
        if (seg.end <= g.from) return;
        var x0 = Math.round(g.x(seg.start)), x1 = Math.max(x0 + 1, Math.round(g.x(seg.end)));
        levelRect(root, { x: x0, y: y0, width: x1 - x0, height: LANE, "fill-opacity": seg.level === "empty" ? 0.95 : 0.8, "shape-rendering": "crispEdges" }, seg.level);
      });
    });
    hatchRect(root, g, inactive ? cursor : dataEnd(g, cursor, limit), g.to, y0, LANE);
  }

  function ignitionEvents(g) {
    return (history.events || []).map(function (ev) {
      return { t: new Date(ev.time).getTime() / 1000,
        failed: ev.kind === "ignite_failed" || ev.kind === "ignite_paused" || /fail/i.test(ev.kind || ""),
        event: ev };
    }).filter(function (ev) {
      return ev.t >= g.from && (ev.event.kind === "ignite" || ev.event.kind === "ignite_manual" || ev.failed);
    });
  }

  function noteNodes(item) {
    var ev = item.event;
    return [document.createTextNode(fmtClock(unixDate(item.t)) + " " + (ev.message || ev.kind || ""))];
  }

  function placeTip(tip, box, g, t, top) {
    var scale = box.clientWidth / g.width, left = g.x(t) * scale + 14;
    if (left + 280 > box.clientWidth) left = g.x(t) * scale - 294;
    tip.style.left = Math.max(0, left) + "px";
    tip.style.top = top + "px";
  }

  function tipRow(tip, text, value, strong) {
    tip.appendChild(el("div", { class: "row" + (strong ? " strong" : "") }, [
      el("span", { class: "key" + (value != null ? " fill-" + quotaLevel(value) : "") }),
      el("span", { class: "name", text: text }),
      el("span", { class: "value", text: value != null ? pct(value) : "无数据" })
    ]));
  }

  // Overview: one band per row on a shared time axis, the ignition lane
  // below, and a crosshair shared with the detail chart.
  function renderOverview(rows, box, end) {
    var g = chartView = geometry(Math.max(480, box.clientWidth || 800), end);
    var limit = gapLimit();
    var flat = [];
    rows.forEach(function (r) {
      flat.push({ row: r, depth: 0 });
      if (r.children && chartExpanded[r.id]) r.children.forEach(function (c) { flat.push({ row: c, depth: 1 }); });
    });
    var height = AXIS_H + flat.length * (LANE + LANE_GAP) + EVENT_H + 6;
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + height, role: "img", "aria-label": "各额度剩余百分比总览" });
    hatchPattern(root);
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      root.appendChild(svgText({ x: g.x(t), y: 13, "text-anchor": "middle", "font-size": 11, fill: cssVar("--muted") }, ticks.label(t)));
      root.appendChild(svg("line", { x1: g.x(t), x2: g.x(t), y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--line"), "stroke-dasharray": "2 4" }));
    });
    var rowY = {};
    flat.forEach(function (f, i) {
      var r = f.row, y0 = AXIS_H + i * (LANE + LANE_GAP);
      rowY[r.id] = y0;
      var selected = chartSelected === r.id;
      if (selected) root.appendChild(svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, rx: 6, fill: cssVar("--chart-select") }));
      var lx = 4 + f.depth * 18;
      if (r.children) root.appendChild(svgText({ x: lx, y: y0 + 15, "font-size": 10, fill: cssVar("--muted") }, chartExpanded[r.id] ? "▾" : "▸"));
      var nameX = lx + (r.children ? 14 : 0);
      var name = f.depth ? r.sub : r.name;
      var maxChars = Math.floor((g.labelW - nameX - (r.children ? 30 : 8)) / 7);
      if (name.length > maxChars) name = name.slice(0, Math.max(1, maxChars - 1)) + "…";
      root.appendChild(svgText({ x: nameX, y: y0 + 15, "font-size": f.depth ? 12 : 13, "font-weight": f.depth ? 400 : 600,
        fill: cssVar(f.depth ? "--muted" : "--text"), "class": "row-name" }, name));
      if (r.children) root.appendChild(svgText({ x: nameX + name.length * 8 + 8, y: y0 + 15, "font-size": 11, fill: cssVar("--muted") }, "×" + r.children.length));
      root.appendChild(svg("rect", { x: g.labelW, y: y0, width: g.plotW, height: LANE, rx: 4, fill: cssVar("--chart-lane") }));
      drawBand(root, g, r.floor, y0, limit, r.inactive);
      var rx = g.width - g.rightW + 14;
      if (r.inactive) {
        root.appendChild(svgText({ x: rx, y: y0 + 15, "font-size": 12, fill: cssVar("--muted"), "class": "row-name" }, "已停用"));
      } else {
        levelRect(root, { x: rx, y: y0 + 7, width: 8, height: 8, rx: 2 }, quotaLevel(r.last));
        root.appendChild(svgText({ x: rx + 14, y: y0 + 15, "font-size": 13, "font-weight": 650, fill: cssVar("--text") }, pct(r.last)));
      }
      if (r.reset && !r.inactive) root.appendChild(svgText({ x: g.width - 2, y: y0 + 15, "font-size": 11, "text-anchor": "end", fill: cssVar("--muted") },
        "↻" + fmtCountdown(r.reset * 1000 - Date.now())));
      var hit = svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, fill: "transparent", style: "cursor:pointer" });
      hit.addEventListener("click", function () {
        if (r.children && chartSelected === r.id) {
          chartExpanded[r.id] = !chartExpanded[r.id];
          saveGroupExpanded(r.id);
        }
        chartSelected = r.id;
        renderChart();
      });
      hit.addEventListener("pointermove", function (e) { hoverAt(e, root, r.id, "overview"); });
      root.appendChild(hit);
    });

    // Ignitions at the same moment merge into one mark with a count.
    var ey = AXIS_H + flat.length * (LANE + LANE_GAP) + 4, cy = ey + 9;
    root.appendChild(svgText({ x: 4, y: ey + 13, "font-size": 12, fill: cssVar("--muted"), "class": "row-name" }, "点火"));
    root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: cy, y2: cy, stroke: cssVar("--line") }));
    var events = ignitionEvents(g), clusters = [];
    events.slice().sort(function (a, b) { return a.t - b.t; }).forEach(function (ev) {
      var last = clusters[clusters.length - 1];
      if (last && g.x(ev.t) - g.x(last.t) < 26) { last.n++; last.failed = last.failed || ev.failed; }
      else clusters.push({ t: ev.t, n: 1, failed: ev.failed });
    });
    clusters.forEach(function (c) {
      var x = g.x(c.t);
      if (c.n > 1) root.appendChild(svgText({ x: x + 6, y: cy + 4, "font-size": 10, fill: cssVar("--muted") }, "×" + c.n));
      if (c.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (cy - 4) + "L" + (x + 4) + " " + (cy + 4) + "M" + (x + 4) + " " + (cy - 4) + "L" + (x - 4) + " " + (cy + 4),
        stroke: cssVar("--danger"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("circle", { cx: x, cy: cy, r: 3.5, fill: cssVar("--ok"), stroke: cssVar("--card"), "stroke-width": 1.5 }));
    });
    var failed = events.filter(function (ev) { return ev.failed; }).length;
    root.appendChild(svgText({ x: g.width - 2, y: ey + 13, "font-size": 11, "text-anchor": "end", fill: cssVar(failed ? "--danger" : "--muted") },
      (events.length - failed) + " 成功" + (failed ? " · " + failed + " 失败" : "")));
    var eventHit = svg("rect", { x: g.labelW, y: ey - 2, width: g.plotW, height: 22, fill: "transparent" });
    eventHit.addEventListener("pointermove", function (e) { hoverAt(e, root, null, "overview"); });
    root.appendChild(eventHit);

    var cross = svg("line", { y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--muted"), "stroke-width": 1, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    hoverListeners.push(function (t, rowId) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      if (hoverSource !== "overview" || t > g.now) { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      function sample(row) {
        var p = valueAt(row.points, t);
        return p && t - p[0] <= limit && !(row.inactive && t > row.lastAt) ? p : null;
      }
      function at(row) { var p = sample(row); return p ? p[1] : null; }
      flat.forEach(function (f) {
        var r = f.row;
        if (f.depth) { tipRow(tip, "　" + r.sub, at(r), r.id === rowId); return; }
        var p = sample(r);
        var suffix = r.children ? " 合计" + (p && p[5] ? "（估算）" : "") : r.inactive ? "（已停用）" : "";
        tipRow(tip, r.name + suffix, p ? p[1] : null, r.id === rowId);
        if (r.children && !chartExpanded[r.id]) {
          r.children.slice(0, 5).forEach(function (c) { tipRow(tip, "　" + c.sub, at(c), false); });
          if (r.children.length > 5) tip.appendChild(el("div", { class: "row muted", text: "　… 另 " + (r.children.length - 5) + " 个账号" }));
        }
      });
      var near = (g.to - g.from) / g.plotW * 8;
      events.forEach(function (ev) {
        if (Math.abs(ev.t - t) <= near) tip.appendChild(el("div", { class: "note" + (ev.failed ? " failed" : "") }, noteNodes(ev)));
      });
      tip.hidden = false;
      var scale = box.clientWidth / g.width;
      placeTip(tip, box, g, t, ((rowY[rowId] != null ? rowY[rowId] : ey) + 28) * scale);
    });
  }

  // Monotone cubic (Fritsch-Carlson): smooth, but never overshoots the
  // samples, so a quota that only falls is never drawn rising.
  function monotonePath(xy) {
    var n = xy.length;
    if (n < 3) return xy.map(function (p, i) { return (i ? "L" : "M") + p[0] + " " + p[1]; }).join("");
    var dx = [], m = [], tg = [], i;
    for (i = 0; i < n - 1; i++) { dx[i] = xy[i + 1][0] - xy[i][0]; m[i] = dx[i] ? (xy[i + 1][1] - xy[i][1]) / dx[i] : 0; }
    tg[0] = m[0]; tg[n - 1] = m[n - 2];
    for (i = 1; i < n - 1; i++) tg[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
    for (i = 0; i < n - 1; i++) {
      if (!m[i]) { tg[i] = tg[i + 1] = 0; continue; }
      var a = tg[i] / m[i], b = tg[i + 1] / m[i], h = a * a + b * b;
      if (h > 9) { var k = 3 / Math.sqrt(h); tg[i] = k * a * m[i]; tg[i + 1] = k * b * m[i]; }
    }
    var d = "M" + xy[0][0] + " " + xy[0][1];
    for (i = 0; i < n - 1; i++) {
      var third = dx[i] / 3;
      d += "C" + (xy[i][0] + third) + " " + (xy[i][1] + tg[i] * third) + " " + (xy[i + 1][0] - third) + " " +
        (xy[i + 1][1] - tg[i + 1] * third) + " " + xy[i + 1][0] + " " + xy[i + 1][1];
    }
    return d;
  }

  // Within one window a quota only falls, but active queries and response
  // headers can disagree by a point. Pool adjacent violators to the closest
  // non-increasing sequence.
  function declining(pts, key) {
    var blocks = [];
    pts.forEach(function (p) {
      blocks.push({ sum: p[key], n: 1 });
      while (blocks.length > 1) {
        var top = blocks[blocks.length - 1], prev = blocks[blocks.length - 2];
        if (top.sum / top.n <= prev.sum / prev.n) break;
        prev.sum += top.sum; prev.n += top.n;
        blocks.pop();
      }
    });
    var out = [], i = 0;
    blocks.forEach(function (b) {
      for (var j = 0; j < b.n; j++, i++) {
        var p = pts[i].slice();
        p[key] = b.sum / b.n;
        out.push(p);
      }
    });
    return out;
  }

  // Keep the first sample of every value, and the last sample of a plateau
  // only when a real drop follows it.
  function curvePoints(pts, key) {
    return pts.filter(function (p, i) {
      if (i === 0 || i === pts.length - 1 || p[key] !== pts[i - 1][key]) return true;
      return Math.abs(pts[i + 1][key] - p[key]) > QUANTUM;
    });
  }

  // Split a line into runs of measured and estimated stretches (index 5 of
  // a pooled point). A stretch is estimated where either end is.
  function estimatedRuns(pts) {
    var runs = [];
    for (var i = 0; i + 1 < pts.length; i++) {
      var estimated = !!(pts[i][5] || pts[i + 1][5]), run = runs[runs.length - 1];
      if (run && run.estimated === estimated) run.pts.push(pts[i + 1]);
      else runs.push({ estimated: estimated, pts: [pts[i], pts[i + 1]] });
    }
    return runs.length ? runs : [{ estimated: !!(pts[0] && pts[0][5]), pts: pts }];
  }

  // Split samples where any of the keys rises by a reset.
  function splitAtResets(pts, keys) {
    var parts = [[pts[0]]], resets = [];
    for (var i = 1; i < pts.length; i++) {
      var up = keys.some(function (k) { return pts[i][k] - pts[i - 1][k] >= RESET_JUMP; });
      if (up && parts.length > 1 && parts[parts.length - 1].length === 1) {
        parts[parts.length - 1] = [];
        resets[resets.length - 1] = pts[i][0];
      } else if (up) {
        parts.push([]);
        resets.push(pts[i][0]);
      }
      parts[parts.length - 1].push(pts[i]);
    }
    for (var k = 0; k + 1 < parts.length; k++) {
      var tail = parts[k][parts[k].length - 1];
      parts[k].push([parts[k + 1][0][0]].concat(tail.slice(1)));
    }
    return { parts: parts, resets: resets };
  }

  var WINDOW_SECONDS = { "5h": 5 * 3600, "7d": 7 * 86400 };
  var RESET_DRIFT = 60;

  // The even pace of one account's window, as segments [a, b, x]: from a to
  // b the pace is 100 × (x − t) / length, falling to 0% at the reset x.
  // resets lists [t, x] in time order: from the sample at t on, the window
  // was due to reset at x, 0 when unknown.
  function paceSegments(resets, length, current, now) {
    var list = (resets || []).filter(function (r) { return Array.isArray(r) && isFinite(r[0]) && isFinite(r[1]); });
    var last = list[list.length - 1];
    var lastKnown = !!last && last[1] > 0;
    if (current != null && isFinite(current) && (!last || now >= last[0])) {
      if (current > 0 ? !lastKnown || Math.abs(current - last[1]) > RESET_DRIFT : lastKnown) list = list.concat([[now, current > 0 ? current : 0]]);
    }
    var out = [];
    if (!(length > 0)) return out;
    list.forEach(function (r, i) {
      var x = r[1];
      if (!(x > 0)) return;
      var next = list[i + 1];
      var a = Math.max(r[0], x - length), b = next ? Math.min(next[0], x) : x;
      if (b > a) out.push([a, b, x, !!next && !(next[1] > 0) && next[0] <= x]);
    });
    return out;
  }

  // Where a line of the even pace starts after its cycle began, it extends
  // back to the start of the cycle: the last rise of the samples by a reset
  // since one window length before its reset.
  function paceExtensions(segments, points, length) {
    var out = [];
    points = points || [];
    segments.forEach(function (s, i) {
      var prev = segments[i - 1];
      if (prev && prev[1] === s[0] && !prev[3] && Math.abs(prev[2] - s[2]) <= RESET_DRIFT) return;
      if (s[0] <= s[2] - length + RESET_DRIFT) return;
      var start = null;
      for (var k = 1; k < points.length && points[k][0] <= s[0]; k++) {
        if (points[k][0] >= s[2] - length - RESET_DRIFT && points[k][1] - points[k - 1][1] >= RESET_JUMP) start = points[k][0];
      }
      if (start != null && start < s[0]) out.push([start, s[0], s[2]]);
    });
    return out;
  }

  function paceAt(segments, length, t) {
    var value = null;
    segments.forEach(function (s) {
      if (t >= s[0] && (s[3] ? t < s[1] : t <= s[1])) value = 100 * (s[2] - t) / length;
    });
    return value;
  }

  function pooledPace(parts, length, t) {
    var sum = 0, total = 0;
    for (var i = 0; i < parts.length; i++) {
      var value = paceAt(parts[i].segments, length, t);
      if (value == null) return null;
      sum += parts[i].weight * value;
      total += parts[i].weight;
    }
    return total ? sum / total : null;
  }

  function extendedPace(parts, length, t) {
    var sum = 0, total = 0, used = false;
    for (var i = 0; i < parts.length; i++) {
      var value = null;
      parts[i].extensions.forEach(function (e) { if (t >= e[0] && t <= e[1]) value = 100 * (e[2] - t) / length; });
      if (value != null) used = true;
      else value = paceAt(parts[i].segments, length, t);
      if (value == null) return null;
      sum += parts[i].weight * value;
      total += parts[i].weight;
    }
    return used && total ? sum / total : null;
  }

  function rowPace(row, now) {
    var length = WINDOW_SECONDS[historyWindow];
    var parts = !length ? [] : (row.children || [row]).map(function (r) {
      var segments = paceSegments(r.resets, length, r.reset, now);
      return { weight: row.children ? r.weight : 1, segments: segments, extensions: paceExtensions(segments, r.points, length) };
    });
    var breaks = [];
    parts.forEach(function (part) {
      part.segments.forEach(function (s) { breaks.push(s[0], s[1]); });
      part.extensions.forEach(function (e) { breaks.push(e[0], e[1]); });
    });
    return { breaks: breaks, at: function (t) { return parts.length ? pooledPace(parts, length, t) : null; },
      extended: function (t) { return parts.length ? extendedPace(parts, length, t) : null; } };
  }

  // The least-squares line through pts[a..b]: its slope per second and the
  // largest distance of a sample from it, in points.
  function lineFit(pts, a, b) {
    var n = b - a + 1, t0 = pts[a][0], st = 0, sv = 0, stt = 0, stv = 0, i, t;
    for (i = a; i <= b; i++) {
      t = pts[i][0] - t0;
      st += t; sv += pts[i][1]; stt += t * t; stv += t * pts[i][1];
    }
    var d = n * stt - st * st, slope = d ? (n * stv - st * sv) / d : 0, base = (sv - slope * st) / n, error = 0;
    for (i = a; i <= b; i++) error = Math.max(error, Math.abs(base + slope * (pts[i][0] - t0) - pts[i][1]));
    return { slope: slope, error: error };
  }

  // The current rate of use, in points per second, from a bottom-up
  // piecewise linear fit: neighbouring pieces merge, the closest fit first,
  // while one line stays within QUANTUM points of all their samples.
  function recentSlope(pts) {
    if (pts.length < 2) return 0;
    var pieces = [], costs = [], i, j;
    for (i = 0; i + 1 < pts.length; i++) pieces.push([i, i + 1]);
    function cost(k) { return lineFit(pts, pieces[k][0], pieces[k + 1][1]).error; }
    for (i = 0; i + 1 < pieces.length; i++) costs.push(cost(i));
    while (costs.length) {
      for (j = 0, i = 1; i < costs.length; i++) if (costs[i] < costs[j]) j = i;
      if (costs[j] > QUANTUM) break;
      pieces.splice(j, 2, [pieces[j][0], pieces[j + 1][1]]);
      costs.splice(j, 1);
      if (j < costs.length) costs[j] = cost(j);
      if (j > 0) costs[j - 1] = cost(j - 1);
    }
    var last = pieces[pieces.length - 1];
    return lineFit(pts, last[0], last[1]).slope;
  }

  // The current rate of a 5-hour window, in points per second: the recent
  // slope of this cycle's lowest readings so far (cycleFloor).
  function currentRate(row, now) {
    var floor = row.floor, start = 0;
    for (var i = 1; i < floor.length; i++) if (floor[i][1] > floor[i - 1][1]) start = i;
    var pts = floor.slice(start), tail = pts[pts.length - 1];
    var held = now - tail[0] > (tail[2] === "passive" ? HOLD_AFTER : Math.max(HOLD_AFTER, sampleStep() * 1.5));
    if (held) pts.push([now, tail[1]]);
    return { rate: recentSlope(pts), held: held };
  }

  // The rate of a 7-day window, in points per second: what this cycle has
  // used so far, over the time since it began. Null when there is none.
  function cycleRate(row, now) {
    var length = WINDOW_SECONDS[historyWindow], sum = 0, total = 0;
    (row.children || [row]).forEach(function (r) {
      var start = r.reset - length, low = r.floor[r.floor.length - 1];
      if (!low || !r.reset || start >= now || r.reset <= now || low[0] < start) return;
      sum += r.weight * (100 - low[1]) / (now - start);
      total += r.weight;
    });
    return total ? -sum / total : null;
  }

  // The projection: from the latest value along the rate of use until the
  // quota runs out (out) or the window resets, with what is left then
  // (left). A 5-hour window keeps its current rate; a 7-day window takes the
  // rate of its cycle so far.
  function projection(row, g, lastRun, limit) {
    var last = row.points[row.points.length - 1];
    if (g.to <= g.now || !lastRun || lastRun.end < g.now - limit || !(last[1] > 0)) return null;
    if (last[5]) return null;
    var rate, t0 = lastRun.end;
    if (historyWindow === "7d") rate = cycleRate(row, g.now);
    else {
      var current = currentRate(row, g.now);
      rate = current.rate;
      if (!current.held) t0 = last[0];
    }
    if (rate == null) return null;
    var slope = Math.min(0, rate), v0 = last[1];
    var out = slope < 0 ? t0 + v0 / -slope : Infinity;
    return { t0: t0, v0: v0, end: Math.min(out, g.to), out: out <= g.to ? out : null, left: out <= g.to ? 0 : v0 + slope * (g.to - t0),
      at: function (t) { return t < t0 || t > g.to ? null : Math.max(0, v0 + slope * (t - t0)); } };
  }

  // Detail: the selected row as a smoothed line that breaks at resets, the
  // even pace by the scheduled resets (rowPace) and, when the axis reaches
  // ahead, the projection at the current rate. A pooled row adds a band from
  // its lowest to its highest account.
  function renderDetail(row, head, box) {
    var g = chartView, limit = gapLimit();
    snapPoints = row.points;
    var group = !!row.children;
    var stretches = runs(row.points, limit, row.inactive ? row.lastAt : g.now);
    var lastRun = stretches[stretches.length - 1];
    var ahead = row.inactive ? null : projection(row, g, lastRun, limit);
    var pieces = stretches.map(function (run) {
      var pts = run.pts.slice(), tail = pts[pts.length - 1], end = ahead && run === lastRun ? ahead.t0 : run.end;
      if (end > tail[0]) pts.push([end].concat(tail.slice(1)));
      return { pts: pts, split: splitAtResets(pts, [1]) };
    });
    var resets = [];
    pieces.forEach(function (piece) { resets = resets.concat(piece.split.resets); });
    var even = row.inactive ? { breaks: [], at: function () { return null; }, extended: function () { return null; } } : rowPace(row, g.now);

    head.appendChild(el("span", { class: "detail-name", text: row.name + " · " + (historyWindow === "5h" ? "5 小时额度" : "7 天额度") }));
    if (row.inactive) head.appendChild(badge("muted", "已停用"));
    head.appendChild(el("span", { class: "muted", text: group ? row.children.length + " 个账号合计，阴影为账号间的最低到最高" : row.sub }));
    if (row.inactive) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode("最后读数"),
        el("strong", { text: pct(row.last) + " · " + fmtDateTime(unixDate(row.lastAt)) })]));
    }
    var pace = even.at(g.now), diff = pace == null ? null : Math.round(row.last - pace);
    (row.inactive ? [] : [[group ? "合计" : "当前", pct(row.last)], ["匀速", pct(pace)],
      ["差值", fmtDiff(diff), diff > 0 ? "ahead" : diff < 0 ? "behind" : ""],
      ["距重置", row.reset ? fmtCountdown(row.reset * 1000 - Date.now()) : "—"]]).forEach(function (stat) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode(stat[0]), el("strong", { class: stat[2] || null, text: stat[1] })]));
    });

    var H = 206, top = 22, bottom = 24, plotH = H - top - bottom;
    function y(v) { return top + (1 - v / 100) * plotH; }
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + H, role: "img", "aria-label": row.name + " 剩余百分比" });
    var defs = hatchPattern(root);
    var fade = svg("linearGradient", { id: "chart-fade", x1: 0, x2: 0, y1: 0, y2: 1 });
    fade.appendChild(svg("stop", { offset: "0%", "stop-color": cssVar("--muted"), "stop-opacity": 0.18 }));
    fade.appendChild(svg("stop", { offset: "100%", "stop-color": cssVar("--muted"), "stop-opacity": 0 }));
    defs.appendChild(fade);
    var clip = svg("clipPath", { id: "chart-plot" });
    clip.appendChild(svg("rect", { x: g.labelW, y: 0, width: g.plotW, height: H }));
    defs.appendChild(clip);
    [0, 50, 100].forEach(function (v) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(v), y2: y(v), stroke: cssVar(v ? "--line" : "--muted"), "stroke-dasharray": v ? "2 4" : "none" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(v) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--muted") }, v + "%"));
    });
    var low = quotaThresholds().low;
    if (low > 0 && low < 100) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(low), y2: y(low), stroke: cssVar("--danger"), "stroke-opacity": 0.5, "stroke-dasharray": "4 4" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(low) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--danger") }, low + "%"));
    }
    var outLabel = null;
    if (ahead && ahead.out != null) {
      var outText = historyWindow === "7d" ? fmtDateTime(unixDate(ahead.out)) : fmtClock(unixDate(ahead.out));
      var outW = outText.length * CHAR_W;
      outLabel = { text: outText, w: outW, x: Math.max(g.labelW + outW / 2, Math.min(g.width - outW / 2, g.x(ahead.out))) };
    }
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      var tx = g.x(t);
      if (tx > g.labelW + 4 && tx < g.labelW + g.plotW - 4) {
        root.appendChild(svg("line", { x1: tx, x2: tx, y1: top, y2: top + plotH, stroke: cssVar("--line"), "stroke-dasharray": "2 4" }));
      }
      var text = ticks.label(t);
      if (outLabel && Math.abs(g.x(t) - outLabel.x) < (outLabel.w + text.length * CHAR_W) / 2 + 6) return;
      root.appendChild(svgText({ x: g.x(t), y: H - 6, "text-anchor": "middle", "font-size": 11, fill: cssVar("--muted") }, text));
    });

    var cursor = g.from;
    stretches.forEach(function (run) {
      if (run.start > cursor + limit) hatchRect(root, g, cursor, run.start, top, plotH);
      cursor = run.end;
    });
    hatchRect(root, g, ahead ? ahead.t0 : row.inactive ? cursor : dataEnd(g, cursor, limit), g.to, top, plotH);
    var firstIn = stretches.filter(function (run) { return run.end > g.from; })[0];
    if (!firstIn || firstIn.start > g.from + limit) {
      root.appendChild(svgText({ x: (g.x(g.from) + g.x(firstIn ? firstIn.start : Math.min(g.now, g.to))) / 2, y: top + plotH / 2 + 4, "text-anchor": "middle", "font-size": 12,
        fill: cssVar("--muted"), "class": "row-name" }, "无数据"));
    }
    [g.labelW, g.labelW + g.plotW].forEach(function (x) {
      root.appendChild(svg("line", { x1: x, x2: x, y1: top, y2: top + plotH, stroke: cssVar("--muted") }));
    });

    var plot = svg("g", { "clip-path": "url(#chart-plot)" });
    root.appendChild(plot);
    var times = [g.from, g.to].concat(even.breaks.filter(function (t) { return t > g.from && t < g.to; })).sort(function (a, b) { return a - b; });
    function pacePath(at) {
      var d = "", end = null;
      for (var k = 0; k + 1 < times.length; k++) {
        var a = times[k], b = times[k + 1], inset = Math.min(1, (b - a) / 4);
        if (b - a <= 0) continue;
        var va = at(a + inset), vb = at(b - inset);
        if (va == null || vb == null) { end = null; continue; }
        if (end == null || Math.abs(end - va) > 0.5) d += "M" + g.x(a) + " " + y(va);
        d += "L" + g.x(b) + " " + y(vb);
        end = vb;
      }
      return d;
    }
    var paceD = pacePath(even.at), extendedD = pacePath(even.extended);
    if (paceD) plot.appendChild(svg("path", { d: paceD, fill: "none", stroke: cssVar("--muted"), "stroke-opacity": 0.8, "stroke-width": 1.5, "stroke-dasharray": "6 4" }));
    if (extendedD) plot.appendChild(svg("path", { d: extendedD, fill: "none", stroke: cssVar("--muted"), "stroke-opacity": 0.8, "stroke-width": 1.5, "stroke-dasharray": "8 3 1.5 3" }));
    $("legend-pace-extended").hidden = !extendedD;
    var estimatedShown = false;
    pieces.forEach(function (piece) {
      if (group) splitAtResets(piece.pts, [3, 4]).parts.forEach(function (part) {
        if (part.length < 2) return;
        var upper = curvePoints(declining(part, 4), 4).map(function (p) { return [g.x(p[0]), y(p[4])]; });
        var lower = curvePoints(declining(part, 3), 3).map(function (p) { return [g.x(p[0]), y(p[3])]; }).reverse();
        plot.appendChild(svg("path", { d: monotonePath(upper) + monotonePath(lower).replace(/^M/, "L") + "Z", fill: cssVar("--muted"), "fill-opacity": 0.13 }));
      });
      piece.split.parts.forEach(function (part) {
        estimatedRuns(declining(part, 1)).forEach(function (run) {
          var xy = curvePoints(run.pts, 1).map(function (p) { return [g.x(p[0]), y(p[1])]; });
          if (xy.length === 1) xy.push([xy[0][0] + 1, xy[0][1]]);
          var d = monotonePath(xy);
          if (!group) plot.appendChild(svg("path", { d: d + "L" + xy[xy.length - 1][0] + " " + y(0) + "L" + xy[0][0] + " " + y(0) + "Z", fill: "url(#chart-fade)" }));
          var line = { d: d, fill: "none", stroke: cssVar("--text"), "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" };
          if (run.estimated) { line["stroke-dasharray"] = "5 4"; line["stroke-opacity"] = 0.7; }
          plot.appendChild(svg("path", line));
          if (run.estimated) estimatedShown = true;
        });
      });
    });
    $("legend-estimated").hidden = !estimatedShown;
    var lastLabel = -99;
    resets.forEach(function (t) {
      if (t < g.from) return;
      var x = g.x(t);
      root.appendChild(svg("line", { x1: x, x2: x, y1: top - 4, y2: top + plotH, stroke: cssVar("--muted"), "stroke-opacity": 0.7, "stroke-dasharray": "3 3" }));
      if (x - lastLabel > 52) {
        root.appendChild(svgText({ x: x, y: top - 8, "text-anchor": "middle", "font-size": 11, fill: cssVar("--muted") }, "↻ " + fmtClock(unixDate(t))));
        lastLabel = x;
      }
    });
    ignitionEvents(g).forEach(function (ev) {
      var x = g.x(ev.t), base = top + plotH;
      if (ev.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (base - 8) + "L" + (x + 4) + " " + base + "M" + (x + 4) + " " + (base - 8) + "L" + (x - 4) + " " + base,
        stroke: cssVar("--danger"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("path", { d: "M" + x + " " + (base - 7) + "L" + (x + 4) + " " + base + "L" + (x - 4) + " " + base + "Z", fill: cssVar("--ok") }));
    });
    var rightX = g.labelW + g.plotW + 10;
    if (ahead) {
      var px = g.x(ahead.end), py = y(ahead.out != null ? 0 : ahead.left);
      root.appendChild(svg("line", { x1: g.x(ahead.t0), y1: y(ahead.v0), x2: px, y2: py, stroke: cssVar("--text"), "stroke-opacity": 0.75,
        "stroke-width": 2, "stroke-dasharray": "1 4", "stroke-linecap": "round" }));
      root.appendChild(svg("circle", { cx: px, cy: py, r: 3, fill: cssVar("--text"), stroke: cssVar("--card"), "stroke-width": 1.5 }));
      if (outLabel) root.appendChild(svgText({ x: outLabel.x, y: H - 6, "text-anchor": "middle", "font-size": 11, "font-weight": 650, fill: cssVar("--text") }, outLabel.text));
      else root.appendChild(svgText({ x: rightX, y: py + 4, "font-size": 12, "font-weight": 650, fill: cssVar("--muted") }, pct(ahead.left)));
    }
    var last = row.points[row.points.length - 1];
    if (last && !row.inactive && lastRun.end >= g.now - limit) {
      root.appendChild(svg("circle", { cx: g.x(ahead ? ahead.t0 : lastRun.end), cy: y(last[1]), r: 4, fill: cssVar("--text"), stroke: cssVar("--card"), "stroke-width": 2 }));
    }
    if (g.to <= g.now && !row.inactive) {
      var yNow = y(row.last), yPace = pace == null ? null : y(pace);
      if (yPace == null) root.appendChild(svgText({ x: rightX, y: yNow + 4, "font-size": 13, "font-weight": 650, fill: cssVar("--text") }, pct(row.last)));
      else {
        var mid = (yNow + yPace) / 2, upper = yNow <= yPace;
        var yA = Math.min(upper ? yNow : yPace, mid - LABEL_H), yB = Math.max(upper ? yPace : yNow, mid + LABEL_H);
        var shift = Math.max(0, top - yA) - Math.max(0, yB - (top + plotH));
        root.appendChild(svgText({ x: rightX, y: (upper ? yA : yB) + shift + 4, "font-size": 13, "font-weight": 650, fill: cssVar("--text") }, pct(row.last)));
        root.appendChild(svgText({ x: rightX, y: (upper ? yB : yA) + shift + 4, "font-size": 12, fill: cssVar("--muted") }, pct(pace)));
        root.appendChild(svgText({ x: rightX, y: mid + shift + 4, "font-size": 12, "font-weight": 650,
          fill: cssVar(diff > 0 ? "--ok" : diff < 0 ? "--danger" : "--muted") }, fmtDiff(diff)));
      }
    }

    var cross = svg("line", { y1: top, y2: top + plotH, stroke: cssVar("--muted"), visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    var dot = svg("circle", { r: 4, fill: cssVar("--text"), stroke: cssVar("--card"), "stroke-width": 2, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(dot);
    var hit = svg("rect", { x: g.labelW, y: top, width: g.plotW, height: plotH, fill: "transparent", tabindex: "0",
      "aria-label": "按左右方向键查看各时刻的数值" });
    hit.addEventListener("pointermove", function (e) { hoverAt(e, root, row.id, "detail"); });
    root.appendChild(hit);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    // Keyboard: step through the samples on the axis.
    var keyIndex = -1, first = 0;
    while (first < row.points.length - 1 && row.points[first][0] < g.from) first++;
    function showSample(i) {
      if (i < first || i >= row.points.length) return;
      keyIndex = i;
      hoverSource = "detail";
      setHover(row.points[i][0], row.id);
    }
    hit.addEventListener("focus", function () { showSample(keyIndex < 0 ? row.points.length - 1 : keyIndex); });
    hit.addEventListener("blur", function () { setHover(null); });
    hit.addEventListener("keydown", function (e) {
      if (e.key === "ArrowLeft") { showSample(Math.max(first, keyIndex - 1)); e.preventDefault(); }
      if (e.key === "ArrowRight") { showSample(Math.min(row.points.length - 1, keyIndex + 1)); e.preventDefault(); }
    });

    function paceRow(t) {
      var value = even.at(t), extended = even.extended(t);
      if (value != null) tip.appendChild(el("div", { class: "row pace" }, [el("span", { class: "key" }),
        el("span", { class: "name", text: "匀速参考" }), el("span", { class: "value", text: pct(value) })]));
      if (extended != null) tip.appendChild(el("div", { class: "row pace extended" }, [el("span", { class: "key" }),
        el("span", { class: "name", text: "匀速参考（往回延长）" }), el("span", { class: "value", text: pct(extended) })]));
    }

    hoverListeners.push(function (t) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); dot.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      if (t > g.now) {
        var projected = ahead ? ahead.at(t) : null;
        if (projected != null) { dot.setAttribute("cx", g.x(t)); dot.setAttribute("cy", y(projected)); dot.setAttribute("visibility", "visible"); }
        else dot.setAttribute("visibility", "hidden");
        if (hoverSource !== "detail") { tip.hidden = true; return; }
        clear(tip);
        tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
        tip.appendChild(el("div", { class: "row projection strong" }, [el("span", { class: "key" }),
          el("span", { class: "name", text: historyWindow === "5h" ? "按当前速度" : "按本周期日均" }), el("span", { class: "value", text: pct(projected) })]));
        paceRow(t);
        tip.hidden = false;
        placeTip(tip, box, g, t, 6);
        return;
      }
      var p = valueAt(row.points, t);
      if (p && (t - p[0] > limit || (row.inactive && t > row.lastAt))) p = null;
      if (p) { dot.setAttribute("cx", g.x(p[0])); dot.setAttribute("cy", y(p[1])); dot.setAttribute("visibility", "visible"); }
      else dot.setAttribute("visibility", "hidden");
      if (hoverSource !== "detail") { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      if (group) {
        tipRow(tip, "合计" + (p && p[5] ? "（估算）" : ""), p ? p[1] : null, true);
        row.children.slice(0, 6).forEach(function (c) {
          var q = valueAt(c.points, t);
          tipRow(tip, "　" + c.sub, q && t - q[0] <= limit ? q[1] : null, false);
        });
        if (row.children.length > 6) tip.appendChild(el("div", { class: "row muted", text: "　… 另 " + (row.children.length - 6) + " 个账号" }));
      } else {
        tipRow(tip, row.name + (p ? (p[2] === "passive" ? "（被动）" : "（主动）") : ""), p ? p[1] : null, true);
      }
      paceRow(t);
      var near = (g.to - g.from) / g.plotW * 8;
      resets.forEach(function (r) { if (r >= g.from && Math.abs(r - t) <= near) tip.appendChild(el("div", { class: "note", text: fmtClock(unixDate(r)) + " 额度重置" })); });
      tip.hidden = false;
      placeTip(tip, box, g, t, 6);
    });
  }

  function hoverAt(e, root, rowId, source) {
    var rect = root.getBoundingClientRect();
    var t = chartView.t((e.clientX - rect.left) / rect.width * chartView.width);
    if (t < chartView.from || t > chartView.to) { setHover(null); return; }
    hoverSource = source;
    setHover(snapTime(t), rowId);
  }

  // The nearest sample of the detail row, so the crosshair and tooltip show a
  // measured time. Inside a data gap and after now the pointer's own time is
  // kept.
  function snapTime(t) {
    if (t > chartView.now) return t;
    var best = null;
    snapPoints.forEach(function (p) {
      if (p[0] >= chartView.from && p[0] <= chartView.to && (best == null || Math.abs(p[0] - t) < Math.abs(best - t))) best = p[0];
    });
    return best != null && Math.abs(best - t) <= gapLimit() ? best : t;
  }

  function setHover(t, rowId) {
    hoverListeners.forEach(function (fn) { fn(t, rowId); });
  }

  // The table covers the requested range, without the sample before it.
  function renderChartTable(rows) {
    var table = $("chart-table");
    var from = new Date(history.from).getTime() / 1000;
    clear(table);
    rows.forEach(function (r) {
      (r.children || [r]).forEach(function (c) {
        var values = c.points.filter(function (p) { return p[0] >= from; }).map(function (p) { return p[1]; });
        table.appendChild(el("tr", {}, [
          el("td", { text: c.name }),
          el("td", { text: c.sub }),
          el("td", { class: "num", text: pct(c.last) }),
          el("td", { class: "num", text: pct(values.length ? Math.min.apply(null, values) : null) }),
          el("td", { class: "num", text: pct(values.length ? Math.max.apply(null, values) : null) }),
          el("td", { class: "num", text: String(values.length) }),
          el("td", { class: "num", text: c.inactive ? "已停用" : c.reset ? fmtCountdown(c.reset * 1000 - Date.now()) : "—" })
        ]));
      });
    });
  }

  // The level swatches of the legend, with the thresholds in use.
  function renderLegend() {
    var t = quotaThresholds();
    $("legend-full").textContent = "满 100%";
    $("legend-high").textContent = "高 > " + t.notice + "%";
    $("legend-medium").textContent = "中 " + t.low + "–" + t.notice + "%";
    $("legend-low").textContent = "低 ≤ " + t.low + "%";
    $("legend-empty").textContent = "空 0%";
  }

  function renderChart() {
    renderLegend();
    // The chart measures its width, so a hidden chart is drawn when its view
    // shows.
    if ($("quota").hidden) return;
    var card = $("chart-card");
    card.style.minHeight = card.offsetHeight + "px";
    try {
      drawChart();
    } finally {
      card.style.minHeight = "";
    }
  }

  function drawChart() {
    var overview = $("chart"), head = $("chart-detail-head"), detail = $("chart-detail");
    chartBehind = false;
    $("legend-estimated").hidden = true;
    overview.classList.toggle("loading", historyLoading !== "" && historyLoading !== historyKey(historyWindow, historyRange));
    clear(overview); clear(head); clear(detail);
    clear($("chart-table"));
    hoverListeners = [];
    snapPoints = [];
    $("chart-detail-wrap").hidden = true;
    if (!history || !status) {
      overview.appendChild(el("div", { class: "empty", text: historyFailed ? "读取历史失败：" + historyFailed : "加载中…" }));
      return;
    }
    var rows = chartRows();
    if (!rows.length) {
      overview.appendChild(el("div", { class: "empty", text: "这个范围内还没有额度数据。" }));
      return;
    }
    if (!findRow(rows, chartSelected)) chartSelected = rows[0].id;
    var selected = findRow(rows, chartSelected);
    renderOverview(rows, overview, axisEnd(selected));
    $("chart-detail-wrap").hidden = false;
    renderDetail(selected, head, detail);
    renderChartTable(rows);
    $("legend-projection").textContent = historyWindow === "5h" ? "按当前速度的延长线" : "按本周期日均的延长线";
  }

  function renderRangeButtons() {
    var box = $("range-buttons");
    var shown = chartWindow;
    if (box.getAttribute("data-shown") !== shown) {
      box.setAttribute("data-shown", shown);
      Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) { box.removeChild(b); });
      RANGES[chartWindow].forEach(function (r) {
        box.appendChild(el("button", { type: "button", class: "seg", "data-value": r[0], text: r[2] === "h" ? r[1] + " 小时" : r[2] === "d" ? r[1] + " 天" : r[1] + " 个月" }));
      });
    }
    pressSegment("range-buttons", range);
  }

  // Chart filters and expanded groups are kept in this browser's
  // localStorage as {window, sort, ranges: {"5h": ..., "7d": ...}} and a
  // list of expanded group names ("all" for the pooled total).
  var CHART_STORE = "phoenix-chart-filters";
  var CHART_EXPANDED_STORE = "phoenix-chart-expanded";

  function expandedGroups() {
    return Object.keys(chartExpanded).filter(function (id) {
      return chartExpanded[id] && id.indexOf("group:") === 0;
    }).map(function (id) { return id.slice(6); });
  }

  function storedExpanded() {
    var saved = null;
    try { saved = JSON.parse(localStorage.getItem(CHART_EXPANDED_STORE) || "null"); } catch (err) {}
    if (!Array.isArray(saved)) return [];
    return saved.filter(function (name) { return typeof name === "string" && name; });
  }

  function writeExpanded(names) {
    try {
      if (names.length) localStorage.setItem(CHART_EXPANDED_STORE, JSON.stringify(names));
      else localStorage.removeItem(CHART_EXPANDED_STORE);
    } catch (err) {}
  }

  function saveGroupExpanded(id) {
    if (id.indexOf("group:") !== 0) return;
    var name = id.slice(6);
    var names = storedExpanded().filter(function (n) { return n !== name; });
    if (chartExpanded[id]) names.push(name);
    writeExpanded(names);
  }

  function restoreChartExpanded() {
    storedExpanded().forEach(function (name) { chartExpanded["group:" + name] = true; });
  }

  function storedChartFilters() {
    try {
      var saved = JSON.parse(localStorage.getItem(CHART_STORE) || "null");
      if (saved && typeof saved === "object") return saved;
    } catch (err) {}
    return {};
  }

  function saveChartFilters() {
    var saved = storedChartFilters();
    var ranges = saved.ranges && typeof saved.ranges === "object" ? saved.ranges : {};
    ranges[chartWindow] = range;
    try {
      localStorage.setItem(CHART_STORE, JSON.stringify({ window: chartWindow, sort: chartSort, ranges: ranges }));
    } catch (err) {}
  }

  function rangeFor(kind) {
    var ranges = storedChartFilters().ranges;
    var saved = ranges && typeof ranges === "object" ? ranges[kind] : null;
    return RANGES[kind].some(function (r) { return r[0] === saved; }) ? saved : DEFAULT_RANGE[kind];
  }

  function restoreChartFilters() {
    restoreChartExpanded();
    var saved = storedChartFilters();
    if (windowKinds().indexOf(saved.window) >= 0) chartWindow = saved.window;
    if (saved.sort === "default" || saved.sort === "low") chartSort = saved.sort;
    range = rangeFor(chartWindow);
    pressSegment("window-buttons", chartWindow);
    pressSegment("sort-buttons", chartSort);
    renderRangeButtons();
  }

  // History responses by window and range. A chosen chart that is cached is
  // drawn at once and redrawn when the new response arrives. historyLoading
  // is the window and range being loaded for the chosen chart, and the chart
  // is grayed out while it differs from the one drawn. Each loadHistory call
  // draws only its own response, and only while it is the latest call.
  var historyCache = {};
  var historyPending = {};
  var historySeq = 0;
  var historyTicket = 0;
  var historyLoading = "";
  var historyFailed = "";

  function historyKey(kind, name) { return kind + "|" + name; }

  // Requests a history, or joins the request for it that is under way unless
  // fresh is set.
  function fetchHistory(kind, name, fresh) {
    var key = historyKey(kind, name);
    if (historyPending[key] && !fresh) return historyPending[key].promise;
    var seq = ++historySeq;
    var entry = { seq: seq };
    var done = function () { if (historyPending[key] === entry) delete historyPending[key]; };
    entry.promise = api("GET", API + "/history?range=" + name + "&window=" + kind).then(function (data) {
      var cached = historyCache[key];
      if (!cached || cached.seq < seq) historyCache[key] = { seq: seq, data: data };
      done();
      return historyCache[key].data;
    }, function (err) {
      done();
      throw err;
    });
    historyPending[key] = entry;
    return entry.promise;
  }

  // The history contract lists resets as [unix_seconds, remaining]: the time
  // a reset happened and the remaining after it. Pace segments need
  // [sample time, reset due], so each reset becomes [reset - period, reset]:
  // during the cycle ending at the reset, that reset was the due one.
  function normalizeHistory(data, kind) {
    // The cached response object is normalized in place; guard against a
    // second pass over the same object.
    if (!data || data._phoenixNormalized) return;
    data._phoenixNormalized = true;
    var period = WINDOW_SECONDS[kind] || 0;
    (data.series || []).forEach(function (s) {
      var resets = Array.isArray(s.resets) ? s.resets : [];
      s.resetTimes = resets.map(function (r) { return r[0]; });
      s.resets = resets.map(function (r) { return [r[0] - period, r[0]]; });
    });
  }

  function showHistory(kind, name, data) {
    normalizeHistory(data, kind);
    history = data;
    historyWindow = kind;
    historyRange = name;
    historyFailed = "";
    renderChart();
  }

  function loadHistory(fresh) {
    var kind = chartWindow, name = range, key = historyKey(kind, name), ticket = ++historyTicket;
    historyLoading = key;
    if (historyCache[key]) showHistory(kind, name, historyCache[key].data);
    else if (historyFailed) { historyFailed = ""; renderChart(); }
    else $("chart").classList.add("loading");
    return fetchHistory(kind, name, fresh).then(function (data) {
      if (ticket !== historyTicket) return false;
      historyLoading = "";
      showHistory(kind, name, data);
      return true;
    }).catch(function (err) {
      if (ticket !== historyTicket) return false;
      historyLoading = "";
      if (err.message !== "unauthorized") toast("读取历史失败：" + err.message);
      if (err.message !== "unauthorized" && historyKey(historyWindow, historyRange) !== key) {
        history = null;
        historyFailed = err.message;
      }
      renderChart();
      return false;
    });
  }

  // The status shows a reading the drawn chart lacks: an active query after
  // the last sample of its window, or a remaining that differs from it.
  function historyBehind() {
    var last = {};
    history.series.forEach(function (s) { if (s.points && s.points.length) last[s.auth_index + "|" + s.window] = s.points[s.points.length - 1]; });
    var loaded = new Date(history.to).getTime() / 1000;
    if (Date.now() / 1000 - loaded > Math.max(300, sampleStep())) return true;
    return (status.accounts || []).some(function (a) {
      return (a.windows || []).some(function (w) {
        var seen = parseTime(w.observed_at);
        if ((w.kind || w.short) !== historyWindow || !seen) return false;
        var t = seen.getTime() / 1000, p = last[a.auth_index + "|" + w.id];
        if (!p) return t > loaded;
        return t >= p[0] + 1 && (w.source === "active" || w.remaining !== p[1]);
      });
    });
  }

  // Loads the drawn chart again when the status shows newer readings. While
  // the pointer is over the chart or a sample has the keyboard focus, a
  // redraw would replace the crosshair and tooltip, so the new data waits.
  var chartCard = $("chart-card");
  var chartPointer = false;
  var chartBehind = false;

  function refreshHistory() {
    if (!history || !status || historyLoading || historyWindow !== chartWindow || historyRange !== range || !historyBehind()) return;
    var ticket = historyTicket;
    fetchHistory(historyWindow, historyRange, true).then(function (data) {
      if (ticket !== historyTicket || historyWindow !== chartWindow || historyRange !== range) return;
      history = data;
      normalizeHistory(history, historyWindow);
      if (chartPointer || chartCard.contains(document.activeElement)) chartBehind = true;
      else renderChart();
    }).catch(function () { /* tried again after the next status */ });
  }

  // After the first chart loads, the other window's chart is fetched so that
  // switching to it does not wait.
  function prefetchHistory() {
    ["5h", "7d"].forEach(function (kind) {
      var name = rangeFor(kind);
      if (kind !== chartWindow && !historyCache[historyKey(kind, name)]) {
        fetchHistory(kind, name).then(function () { if (status && renderWindowButtons()) loadHistory(); }).catch(function () {});
      }
    });
  }

  // ---- Settings ----
  function setField(input, value) {
    if (!input) return;
    if (input.settingsDirty || document.activeElement === input) return;
    if (input.type === "checkbox") input.checked = !!value;
    else input.value = value == null ? "" : value;
  }

  function setSecret(input, isSet, placeholder) {
    if (!input) return;
    if (input.settingsDirty || document.activeElement === input) return;
    input.value = "";
    input.placeholder = isSet ? "已设置（留空不修改）" : placeholder;
  }

  function setRadio(name, value) {
    var inputs = document.querySelectorAll('input[name="' + name + '"]');
    var dirty = false;
    Array.prototype.forEach.call(inputs, function (i) { if (i.settingsDirty) dirty = true; });
    if (dirty) return;
    Array.prototype.forEach.call(inputs, function (i) { i.checked = i.value === value; });
  }

  function getRadio(name) {
    var hit = document.querySelector('input[name="' + name + '"]:checked');
    return hit ? hit.value : "off";
  }

  // Fills the settings from status.config. The ignition model is rendered
  // verbatim from the config, never reset to a default by the page. Secrets
  // arrive as "" with a *_set flag and are rendered empty with a placeholder.
  function fillSettings() {
    if (!status) return;
    var cfg = status.config || {};
    var tg = cfg.telegram || {};
    setSecret($("bot-token"), !!tg.bot_token_set, "123456:ABC...");
    setField($("chat-ids"), tg.chat_ids || "");
    setField($("telegram-enabled"), !!tg.enabled);
    var rs = cfg.reset || {};
    setField($("reset-enabled"), rs.enabled !== false);
    setField($("weekly-only"), rs.weekly_only !== false);
    setField($("min-reset"), rs.minimum_reset_seconds != null ? rs.minimum_reset_seconds : 86400);
    setField($("reset-credentials"), (rs.credentials || []).join(";"));
    var al = cfg.alerts || {};
    var th = Array.isArray(al.thresholds) ? al.thresholds : [50, 20, 10];
    setField($("notice-threshold"), th[0] != null ? th[0] : 50);
    setField($("low-threshold"), th[1] != null ? th[1] : 20);
    setField($("critical-threshold"), th[2] != null ? th[2] : 10);
    var rec = al.recovery_notify || null;
    setRadio("recovery.five_hour", rec && rec.five_hour ? rec.five_hour : (al.notify_recovery ? "all" : "off"));
    setRadio("recovery.seven_day", rec && rec.seven_day ? rec.seven_day : (al.notify_recovery ? "all" : "off"));
    var rem = al.reset_reminder || null;
    setRadio("reminder.five_hour", rem && rem.five_hour ? rem.five_hour : "off");
    setRadio("reminder.seven_day", rem && rem.seven_day ? rem.seven_day : "off");
    setField($("notify-errors"), !!al.notify_errors);
    var ig = cfg.ignition || {};
    setField($("ignition-enabled"), ig.enabled !== false);
    setField($("start-hour"), ig.start_hour != null ? ig.start_hour : 7);
    setField($("end-hour"), ig.end_hour != null ? ig.end_hour : 22);
    setField($("end-grace"), ig.end_grace_minutes != null ? ig.end_grace_minutes : 30);
    setField($("grace"), ig.grace_seconds != null ? ig.grace_seconds : 3);
    setField($("failure-retry"), ig.failure_retry_seconds != null ? ig.failure_retry_seconds : 300);
    setField($("max-transient"), ig.max_transient_failures != null ? ig.max_transient_failures : 3);
    setField($("ignition-model"), ig.model != null ? ig.model : "");
    setField($("cpa-base-url"), ig.cpa_base_url != null ? ig.cpa_base_url : "");
    setField($("poll-interval"), cfg.poll_interval_seconds != null ? cfg.poll_interval_seconds : 300);
    setField($("history-retention"), cfg.history_retention_days != null ? cfg.history_retention_days : 30);
    setSecret($("models-api-key"), !!ig.models_api_key_set, "用于读取 CPA 模型列表");
    renderIgnitionOff();
    settingsFilled = true;
  }

  function renderIgnitionOff() {
    var input = $("ignition-enabled");
    var off = !(input && input.checked);
    $("ignition-params").classList.toggle("off", off);
    $("ignition-off").hidden = !off;
  }

  function splitList(v) {
    return String(v || "").split(/[;,]/).map(function (x) { return x.trim(); }).filter(Boolean);
  }

  function numValue(input, fallback) {
    var v = Number(input.value);
    return isFinite(v) ? v : fallback;
  }

  // The patch for one settings section, built from the current form values
  // on top of the config the status reports, so keys the page does not show
  // are kept. Empty secret fields are omitted, so the backend keeps the
  // stored value instead of clearing it.
  function buildPatch(section) {
    var cfg = status.config || {};
    if (section === "telegram") {
      var tg = JSON.parse(JSON.stringify(cfg.telegram || {}));
      tg.enabled = $("telegram-enabled").checked;
      tg.chat_ids = $("chat-ids").value;
      var bot = $("bot-token").value.trim();
      if (bot) tg.bot_token = bot; else delete tg.bot_token;
      return { telegram: tg };
    }
    if (section === "reset") {
      var rs = JSON.parse(JSON.stringify(cfg.reset || {}));
      rs.enabled = $("reset-enabled").checked;
      rs.weekly_only = $("weekly-only").checked;
      rs.minimum_reset_seconds = numValue($("min-reset"), 86400);
      rs.credentials = splitList($("reset-credentials").value);
      return { reset: rs };
    }
    if (section === "alerts") {
      var al = JSON.parse(JSON.stringify(cfg.alerts || {}));
      al.thresholds = [numValue($("notice-threshold"), 50), numValue($("low-threshold"), 20), numValue($("critical-threshold"), 10)];
      var rec5 = getRadio("recovery.five_hour"), rec7 = getRadio("recovery.seven_day");
      al.recovery_notify = { five_hour: rec5, seven_day: rec7 };
      al.notify_recovery = rec5 !== "off" || rec7 !== "off";
      al.reset_reminder = { five_hour: getRadio("reminder.five_hour"), seven_day: getRadio("reminder.seven_day") };
      al.notify_errors = $("notify-errors").checked;
      return { alerts: al };
    }
    if (section === "ignition") {
      var ig = JSON.parse(JSON.stringify(cfg.ignition || {}));
      ig.enabled = $("ignition-enabled").checked;
      ig.start_hour = numValue($("start-hour"), 7);
      ig.end_hour = numValue($("end-hour"), 22);
      ig.end_grace_minutes = numValue($("end-grace"), 30);
      ig.grace_seconds = numValue($("grace"), 3);
      ig.failure_retry_seconds = numValue($("failure-retry"), 300);
      ig.max_transient_failures = numValue($("max-transient"), 3);
      var model = $("ignition-model").value.trim();
      if (model) ig.model = model;
      else if (cfg.ignition && cfg.ignition.model) ig.model = "";
      else delete ig.model;
      var mk = $("models-api-key").value.trim();
      if (mk) ig.models_api_key = mk; else delete ig.models_api_key;
      var cb = $("cpa-base-url").value.trim();
      if (cb) ig.cpa_base_url = cb; else delete ig.cpa_base_url;
      return { ignition: ig };
    }
    if (section === "advanced") {
      return { poll_interval_seconds: numValue($("poll-interval"), 300), history_retention_days: numValue($("history-retention"), 30) };
    }
    return {};
  }

  function thresholdsInOrder() {
    var values = ["notice-threshold", "low-threshold", "critical-threshold"].map(function (id) {
      return $(id).value === "" ? NaN : Number($(id).value);
    });
    return values.every(isFinite) && values[0] >= values[1] && values[1] >= values[2];
  }

  // ---- Save queue + applied confirmation ----
  // Settings save serially. Each section owns its result; a late reply
  // cannot label a newer edit.
  var saveQueue = Promise.resolve();
  var saveOwners = {};

  function setSaveState(section, state, text) {
    var node = $("save-" + section);
    if (!node) return;
    node.setAttribute("data-state", state || "");
    node.textContent = text || "";
  }

  function claimState(section) {
    var owner = {};
    saveOwners[section] = owner;
    setSaveState(section, "waiting", "保存中…");
    return function (state, text) {
      if (saveOwners[section] !== owner) return false;
      setSaveState(section, state, text);
      return true;
    };
  }

  function editSaveState(section) {
    var node = $("save-" + section);
    if (!node) return;
    if (node.getAttribute("data-state") === "failed") return;
    saveOwners[section] = null;
    setSaveState(section, "", "");
  }

  // Every config write carries a new config_token. The plugin reports, as
  // config_applied_token in the status, the token of the config it has
  // applied. A save counts as applied when that token is its own or one
  // this page saved after it.
  var savedTokens = [];
  var applyWaits = [];
  var applyTimer = null;
  var APPLY_TIMEOUT = 15000;
  var POLL_TIMEOUT = 4000;

  // 16 random hexadecimal digits.
  function newConfigToken() {
    var bytes = new Uint8Array(8);
    crypto.getRandomValues(bytes);
    var text = "";
    for (var i = 0; i < bytes.length; i++) text += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
    return text;
  }

  function tokenApplied(tokens, token, applied) {
    var saved = tokens.indexOf(token);
    return saved >= 0 && !!applied && tokens.indexOf(applied) >= saved;
  }

  function lastToken() { return savedTokens.length ? savedTokens[savedTokens.length - 1] : ""; }

  // Writes a patch of the plugin config with a new token, and resolves to
  // the token once CPA has saved it. Falls back to PUT /settings only when
  // the PATCH endpoint answers 404/405.
  function patchConfig(patch) {
    var token = newConfigToken(), body = {};
    Object.keys(patch).forEach(function (name) { body[name] = patch[name]; });
    body.config_token = token;
    return api("PATCH", CONFIG_API, body).then(function () {
      savedTokens.push(token);
      if (savedTokens.length > 200) savedTokens.shift();
      return { token: token, fallback: false };
    }, function (err) {
      if (err && (err.status === 404 || err.status === 405)) {
        var fb = {};
        Object.keys(patch).forEach(function (name) { fb[name] = patch[name]; });
        return api("PUT", API + "/settings", fb).then(function () { return { token: token, fallback: true }; });
      }
      throw err;
    });
  }

  function whenApplied(token) {
    if (tokenApplied(savedTokens, token, status && status.config_applied_token)) return Promise.resolve(true);
    return new Promise(function (resolve) {
      var wait = { token: token };
      wait.done = function (applied) {
        clearTimeout(wait.timer);
        applyWaits = applyWaits.filter(function (w) { return w !== wait; });
        resolve(applied);
      };
      wait.timer = setTimeout(function () { wait.done(false); }, APPLY_TIMEOUT);
      applyWaits.push(wait);
      pollApplied();
    });
  }

  // Settles the waiting saves that the status shows as applied. Called with
  // every status.
  function checkApplied() {
    var applied = status && status.config_applied_token;
    applyWaits.slice().forEach(function (w) {
      if (tokenApplied(savedTokens, w.token, applied)) w.done(true);
    });
  }

  function pollApplied() {
    if (applyTimer || !applyWaits.length) return;
    applyTimer = setTimeout(function () {
      loadStatus(POLL_TIMEOUT).catch(function () {}).then(function () {
        applyTimer = null;
        checkApplied();
        pollApplied();
      });
    }, 1000);
  }

  // The save state after CPA saved it: waiting until the plugin applies it,
  // then applied, or unconfirmed after the timeout. A fallback save counts
  // as saved at once, since it carries no token to confirm.
  function showApplied(say, token, fallback) {
    if (fallback) { say("saved", "已保存"); return Promise.resolve(true); }
    say("waiting", "已保存，等待生效");
    return whenApplied(token).then(function (applied) {
      say(applied ? "saved" : "unverified", applied ? "已生效" : "未能验证");
      return applied;
    });
  }

  function captureEdits(section) {
    var map = new Map();
    var box = document.querySelector('.settings-section[data-section="' + section + '"]');
    if (box) Array.prototype.forEach.call(box.querySelectorAll("input"), function (input) { map.set(input, input.settingsEdit || 0); });
    return map;
  }

  // Clear only drafts actually submitted by this save and unchanged since
  // its intent was captured, then refill from the freshly applied config.
  function refillSaved(section, edits) {
    var box = document.querySelector('.settings-section[data-section="' + section + '"]');
    if (box) Array.prototype.forEach.call(box.querySelectorAll("input"), function (input) {
      if (edits && edits.get(input) === (input.settingsEdit || 0)) input.settingsDirty = false;
    });
    fillSettings();
  }

  function saveSection(section) {
    if (section === "alerts" && !thresholdsInOrder()) {
      ["notice-threshold", "low-threshold", "critical-threshold"].forEach(function (id) { $(id).setAttribute("aria-invalid", "true"); });
      setSaveState(section, "failed", "阈值须满足：提醒 ≥ 偏低 ≥ 告急，未保存");
      return;
    }
    if (section === "alerts") {
      ["notice-threshold", "low-threshold", "critical-threshold"].forEach(function (id) { $(id).removeAttribute("aria-invalid"); });
    }
    var say = claimState(section);
    var patch, edits;
    try {
      patch = buildPatch(section);
      edits = captureEdits(section);
    } catch (err) { say("failed", err.message); return; }
    saveQueue = saveQueue.then(function () {
      return patchConfig(patch).then(function (res) {
        return showApplied(say, res.token, res.fallback).then(function (applied) {
          if (applied) refillSaved(section, edits);
          return loadStatus().catch(function () {});
        });
      }).catch(function (err) {
        say("failed", err.message);
      });
    });
  }

  // autofilled reports whether the browser filled in the value. Firefox and
  // recent Chrome know :autofill; Safari and older Chrome only the prefixed
  // name.
  function autofilled(input) {
    try { if (input.matches(":autofill")) return true; } catch (err) {}
    try { return input.matches(":-webkit-autofill"); } catch (err) { return false; }
  }

  // One line per channel.
  function renderTestResults(results) {
    var box = $("test-results");
    clear(box);
    (results || []).forEach(function (r) {
      var children = [el("span", { text: r.ok ? r.channel + "：已发送" : r.channel + "：失败，" + (r.error || "") })];
      if (r.ok && r.response) children.push(el("span", { class: "hint", text: "响应：" + r.response }));
      if (r.ok && r.unchecked) children.push(el("span", { class: "hint", text: "只检查了 HTTP 状态码，请到客户端确认是否收到。" }));
      box.appendChild(el("div", { class: "test-result " + (r.ok ? "ok" : "failed") }, children));
    });
  }

  // ---- Startup ----
  function start() {
    $("key-section").hidden = true;
    loadStatus().then(function () {
      restoreChartFilters();
      return loadHistory();
    }).then(function (loaded) { if (loaded) prefetchHistory(); }).catch(function (err) {
      if (err.message !== "unauthorized") toast("加载失败：" + err.message);
    });
    clearInterval(refreshTimer);
    refreshTimer = setInterval(function () {
      if (document.hidden) return;
      loadStatus().then(refreshHistory).catch(function () {});
    }, 30000);
  }

  var VIEW_STORE = "phoenix-view";
  function showView(name) {
    var settings = name === "settings";
    $("quota").hidden = settings;
    $("settings").hidden = !settings;
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (b) {
      b.classList.toggle("active", b.dataset.view === name);
    });
    try { localStorage.setItem(VIEW_STORE, settings ? "settings" : "quota"); } catch (err) {}
    if (!settings && history && status) renderChart();
  }

  function pressSegment(id, value) {
    var box = $(id);
    Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-value") === value ? "true" : "false");
    });
  }

  // Delegated, so it also covers buttons added later. Clicking the pressed
  // button only retries a chart that failed to load.
  function bindSegmented(id, onPick) {
    $(id).addEventListener("click", function (e) {
      var button = e.target.closest(".seg");
      if (!button) return;
      if (button.getAttribute("aria-pressed") === "true") {
        if (historyFailed) loadHistory();
        return;
      }
      var value = button.getAttribute("data-value");
      pressSegment(id, value);
      onPick(value);
    });
  }

  bindSegmented("range-buttons", function (value) {
    range = value;
    saveChartFilters();
    loadHistory();
  });
  bindSegmented("window-buttons", function (value) {
    chartWindow = value;
    range = rangeFor(value);
    saveChartFilters();
    renderRangeButtons();
    loadHistory();
  });
  bindSegmented("sort-buttons", function (value) {
    chartSort = value;
    saveChartFilters();
    renderChart();
  });

  Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (b) {
    b.onclick = function () { showView(b.dataset.view); };
  });
  $("refresh").addEventListener("click", function (e) {
    runAction(e.currentTarget, API + "/refresh", {}, "已刷新全部额度");
  });
  $("events-more").addEventListener("click", function () { eventsExpanded = !eventsExpanded; renderEvents(); });
  $("events-5h").addEventListener("change", function (e) {
    showFiveHourResets = e.target.checked;
    try { localStorage.setItem(EVENTS_STORE, showFiveHourResets ? "1" : "0"); } catch (err) {}
    if (status) renderEvents();
  });
  $("test").addEventListener("click", function (e) {
    // The test waits for the saves under way and for the plugin to apply
    // the latest of them. When it is not applied in time, the test is not
    // sent, since it would use the settings before the change.
    var button = e.currentTarget;
    button.disabled = true;
    var results = $("test-results");
    clear(results);
    saveQueue.then(function () {
      var token = lastToken();
      if (!token || tokenApplied(savedTokens, token, status && status.config_applied_token)) return true;
      results.appendChild(el("div", { class: "test-result hint", text: "等待插件应用新设置…" }));
      return whenApplied(token);
    }).then(function (applied) {
      if (!applied) {
        clear(results);
        results.appendChild(el("div", { class: "test-result failed", text: "配置尚未生效，未发送测试" }));
        return null;
      }
      return api("POST", API + "/test-notify", {});
    }).then(function (data) {
      if (!data) return;
      if (data.status) { status = data.status; renderStatus(); }
      renderTestResults(data.results);
      if (data.ok) toast("测试通知已发送");
      else if (!data.results) toast("失败：" + (data.error || ""));
      else toast(data.results.some(function (r) { return r.ok; }) ? "部分渠道测试失败" : "测试推送失败");
    }).catch(function (err) {
      clear(results);
      if (err.message !== "unauthorized") toast("失败：" + err.message);
    }).then(function () { button.disabled = false; });
  });

  var settingsForm = $("settings-form");
  settingsForm.addEventListener("change", function (e) {
    var input = e.target;
    var sectionEl = input.closest(".settings-section");
    if (!sectionEl) return;
    var section = sectionEl.dataset.section;
    if (input.type === "password") {
      // A value the browser fills in while the field is not focused comes
      // from autofill on page load; it is not saved, and the field shows
      // the saved value again. The management key is refused with a message.
      if (autofilled(input) && document.activeElement !== input) { fillSettings(); return; }
      if (input.id === "models-api-key" && key && input.value.trim() === key) {
        setSaveState(section, "failed", "这是 CPA 管理密钥，未保存。请填写 access.api-keys 中的 key");
        return;
      }
    }
    if (input.type === "number") {
      if (!input.checkValidity()) {
        setSaveState(section, "failed", input.validationMessage);
        input.reportValidity();
        return;
      }
      if (input.value === "") { fillSettings(); return; }
    }
    if (input.id === "ignition-enabled") renderIgnitionOff();
    saveSection(section);
  });
  // Keep failed drafts visible while invalidating responses for older edits.
  settingsForm.addEventListener("input", function (e) {
    var input = e.target;
    input.settingsDirty = true;
    input.settingsEdit = (input.settingsEdit || 0) + 1;
    var sectionEl = input.closest(".settings-section");
    if (sectionEl) editSaveState(sectionEl.dataset.section);
  });
  settingsForm.addEventListener("submit", function (e) { e.preventDefault(); });
  // Enter commits a text or number field; leaving it fires the change that saves it.
  settingsForm.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && e.target.tagName === "INPUT" && /^(text|number|password)$/.test(e.target.type)) {
      e.preventDefault();
      e.target.blur();
    }
  });

  chartCard.addEventListener("pointerenter", function () { chartPointer = true; });
  chartCard.addEventListener("pointerleave", function () {
    chartPointer = false;
    if (chartBehind && !chartCard.contains(document.activeElement)) renderChart();
  });
  chartCard.addEventListener("focusout", function (e) {
    if (chartBehind && !chartPointer && !chartCard.contains(e.relatedTarget)) renderChart();
  });
  window.addEventListener("resize", function () { if (history) renderChart(); });

  $("key-ok").onclick = function () {
    var v = $("mgmt-key").value.trim();
    if (!v) { $("key-error").textContent = "请先输入管理密钥。"; return; }
    key = v;
    try { sessionStorage.setItem(KEY_STORE, v); } catch (e) {}
    $("key-section").hidden = true;
    $("mgmt-key").value = "";
    start();
  };

  renderRangeButtons();
  var storedView = "quota";
  try { storedView = localStorage.getItem(VIEW_STORE) || "quota"; } catch (err) {}
  showView(storedView);
  if (key) start(); else askForKey("");
})();
