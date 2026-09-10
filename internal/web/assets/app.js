/* Lawyer CRM — Admin single-page application.
 *
 * Dependency-free ES module. Every visible control is wired to a real endpoint;
 * there are no placeholder buttons. All state changes go through the server,
 * and the browser never holds a provider credential of any kind.
 */

const API = "./api";

/* ------------------------------------------------------------------ utils */

const el = (tag, attrs = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "html") node.innerHTML = v;
    else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
};

const clear = (node) => { while (node.firstChild) node.removeChild(node.firstChild); return node; };

const readCookie = (name) => {
  const match = document.cookie.match(new RegExp("(^| )" + name + "=([^;]+)"));
  return match ? decodeURIComponent(match[2]) : "";
};

function fmtTime(value) {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString([], { day: "2-digit", month: "2-digit" }) + " " +
      d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function fmtDate(value) {
  if (!value) return "—";
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleDateString([], { day: "2-digit", month: "2-digit", year: "numeric" });
}

function relative(value) {
  if (!value) return "—";
  const diff = Date.now() - new Date(value).getTime();
  if (Number.isNaN(diff)) return "—";
  const min = Math.round(diff / 60000);
  if (min < 1) return "только что";
  if (min < 60) return `${min} мин назад`;
  const hours = Math.round(min / 60);
  if (hours < 24) return `${hours} ч назад`;
  return `${Math.round(hours / 24)} д назад`;
}

function fmtBytes(n) {
  if (!n) return "";
  const units = ["Б", "КБ", "МБ", "ГБ"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

/* ------------------------------------------------------------------- http */

class HttpError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function request(path, options = {}) {
  const opts = { credentials: "same-origin", headers: {}, ...options };
  if (!(opts.body instanceof FormData) && opts.body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  if (!["GET", "HEAD"].includes((opts.method || "GET").toUpperCase())) {
    opts.headers["X-CSRF-Token"] = state.csrf || readCookie("lb_csrf");
  }

  const res = await fetch(`${API}${path}`, opts);
  if (res.status === 401) { state.user = null; render(); throw new HttpError(401, "Сессия истекла"); }

  const type = res.headers.get("content-type") || "";
  if (!type.includes("application/json")) {
    if (!res.ok) throw new HttpError(res.status, `Ошибка ${res.status}`);
    return res;
  }
  const body = await res.json();
  if (!res.ok) throw new HttpError(res.status, body.error || `Ошибка ${res.status}`);
  return body;
}

const get = (path) => request(path);
const post = (path, body) => request(path, { method: "POST", body: body ?? {} });
const patch = (path, body) => request(path, { method: "PATCH", body });
const del = (path) => request(path, { method: "DELETE" });

/* ------------------------------------------------------------------ state */

const state = {
  user: null,
  csrf: "",
  version: "",
  booted: false,
  route: { name: "dashboard", params: {} },
  statuses: [],
  services: [],
  consultants: [],
  settings: null,
  counts: { unread: 0, needsConsultant: 0, followUps: 0 },
  filters: { q: "", status: "", service: "", language: "", mode: "", assigned: "", blocked: "", from: "", to: "", sort: "last_activity", dir: "desc", limit: 50, offset: 0 },
  clients: { items: [], total: 0, loading: false },
  client: null,
  messages: [],
  notes: [],
  hasMoreMessages: false,
  attachment: null,
  stream: null,
  busy: new Set(),
};

/* ----------------------------------------------------------------- toasts */

const toastHost = el("div", { class: "toasts" });

function toast(message, kind = "") {
  const node = el("div", { class: `toast ${kind}` }, message);
  toastHost.append(node);
  setTimeout(() => node.remove(), kind === "err" ? 6000 : 3200);
}

/* ------------------------------------------------------------------ modal */

function confirmDialog({ title, body, confirmLabel = "Подтвердить", danger = false, field = null }) {
  return new Promise((resolve) => {
    let input = null;
    if (field) input = el("input", { type: "text", placeholder: field.placeholder || "" });

    const close = (value) => { backdrop.remove(); resolve(value); };
    const backdrop = el("div", { class: "modal-backdrop", onclick: (e) => { if (e.target === backdrop) close(null); } },
      el("div", { class: "modal" },
        el("h3", {}, title),
        el("div", { class: "modal-body" },
          el("p", { style: "margin:0 0 10px" }, body),
          field ? el("label", {}, el("div", { class: "hint", style: "margin-bottom:4px" }, field.label), input) : null),
        el("div", { class: "modal-foot" },
          el("button", { class: "btn", onclick: () => close(null) }, "Отмена"),
          el("button", { class: `btn ${danger ? "danger" : "primary"}`, onclick: () => close(field ? (input.value || "") : true) }, confirmLabel))));

    document.body.append(backdrop);
    (input || backdrop.querySelector(".modal-foot .btn:last-child")).focus();
    document.addEventListener("keydown", function esc(e) {
      if (e.key === "Escape") { document.removeEventListener("keydown", esc); close(null); }
    });
  });
}

/* ------------------------------------------------------------------ login */

function LoginView() {
  const email = el("input", { type: "email", required: true, autocomplete: "username" });
  const password = el("input", { type: "password", required: true, autocomplete: "current-password" });
  const error = el("div", { class: "error-box", style: "display:none" });
  const submit = el("button", { class: "btn primary", type: "submit" }, "Войти");

  const form = el("form", {
    onsubmit: async (e) => {
      e.preventDefault();
      error.style.display = "none";
      submit.disabled = true;
      submit.textContent = "Проверяем…";
      try {
        const res = await post("/auth/login", { email: email.value.trim(), password: password.value });
        state.user = res.user;
        state.csrf = res.csrf_token;
        await bootstrap();
        navigate("dashboard");
      } catch (err) {
        error.textContent = err.message;
        error.style.display = "block";
        password.value = "";
        password.focus();
      } finally {
        submit.disabled = false;
        submit.textContent = "Войти";
      }
    },
  },
    el("h1", {}, "Lawyer CRM"),
    el("p", { class: "sub" }, "Панель консультанта"),
    error,
    el("label", {}, el("span", {}, "Email"), email),
    el("label", {}, el("span", {}, "Пароль"), password),
    submit);

  setTimeout(() => email.focus(), 0);
  return el("div", { class: "login-wrap" }, el("div", { class: "login" }, form));
}

/* ------------------------------------------------------------------ shell */

const NAV = [
  { id: "dashboard", label: "Дашборд" },
  { id: "clients", label: "Клиенты", count: () => state.counts.unread },
  { id: "conversations", label: "Диалоги", count: () => state.counts.needsConsultant },
  { id: "followups", label: "Напоминания", count: () => state.counts.followUps },
  { id: "consultants", label: "Консультанты" },
  { id: "services", label: "Услуги" },
  { id: "analytics", label: "Аналитика" },
  { id: "exports", label: "Экспорт" },
  { id: "settings", label: "Настройки" },
];

function Sidebar() {
  const nav = el("nav", { class: "nav" });
  for (const item of NAV) {
    const n = item.count ? item.count() : 0;
    nav.append(el("a", {
      href: `#/${item.id}`,
      class: state.route.name === item.id ? "active" : "",
    }, el("span", {}, item.label), n > 0 ? el("span", { class: "count" }, String(n)) : null));
  }
  nav.append(el("a", {
    href: "#",
    onclick: async (e) => {
      e.preventDefault();
      try { await post("/auth/logout"); } catch { /* the session is gone either way */ }
      state.user = null; state.csrf = ""; stopStream(); render();
    },
  }, el("span", {}, "Выйти")));

  return el("aside", { class: "sidebar" },
    el("div", { class: "brand" }, el("span", { class: `dot ${state.stream ? "" : "off"}` }), "Lawyer CRM"),
    BotControl(),
    nav,
    el("div", { class: "sidebar-foot" },
      el("div", { class: "who" }, state.user?.name || state.user?.email || ""),
      el("div", {}, roleLabel(state.user?.role), state.version ? ` · v${state.version}` : "")));
}

// setBotEnabled drives the persisted automation switch. The browser never
// decides what the switch shows: the value rendered afterwards is the one the
// server returned, and a failed write is repaired from the server too.
async function setBotEnabled(next) {
  if (state.busy.has("whatsapp-bot")) return;
  state.busy.add("whatsapp-bot");
  try {
    state.settings = await patch("/settings", { whatsapp_bot_enabled: next });
    toast(`WhatsApp Bot ${next ? "ON" : "OFF"}`, "ok");
  } catch (err) {
    toast(err.message, "err");
    try { state.settings = await get("/settings"); } catch { /* keep the last confirmed payload */ }
  } finally {
    state.busy.delete("whatsapp-bot");
  }
  await render();
}

function botConnectionLabel(status) {
  return { connected: "подключён", disconnected: "нет связи" }[status] || "нет данных";
}

function BotControl() {
  const wa = state.settings?.whatsapp || {};
  const known = typeof wa.bot_enabled === "boolean";
  const enabled = known ? wa.bot_enabled : true;
  const busy = state.busy.has("whatsapp-bot");
  const toggle = el("button", {
    class: `bot-switch ${enabled ? "on" : "off"}`,
    disabled: busy || !known,
    title: known ? "Автоответы ассистента" : "Настройки не загружены",
    onclick: () => setBotEnabled(!enabled),
  }, el("span", {}, enabled ? "ON" : "OFF"));

  return el("div", { class: "bot-card" },
    el("div", { class: "bot-title" }, "WhatsApp Bot"),
    toggle,
    el("div", { class: "bot-status" }, "Подключение: ", botConnectionLabel(wa.connection_status)));
}

const roleLabel = (role) => ({ admin: "Администратор", consultant: "Консультант", readonly: "Только чтение" }[role] || role || "");

function Topbar(title, ...extra) {
  return el("header", { class: "topbar" }, el("h1", {}, title), el("div", { class: "grow" }), ...extra);
}

/* -------------------------------------------------------------- dashboard */

async function DashboardView() {
  const board = await get("/dashboard");
  state.counts.unread = board.unread || 0;
  state.counts.needsConsultant = board.by_status?.needs_consultant || 0;
  state.counts.followUps = board.scheduled_follow_ups || 0;

  const byStatus = board.by_status || {};
  const metric = (label, value, filter, cls = "") =>
    el("button", { class: `metric ${cls}`, onclick: () => openClients(filter) },
      el("div", { class: "label" }, label),
      el("div", { class: "value" }, String(value ?? 0)));

  const metrics = el("div", { class: "metrics" },
    metric("Новые сегодня", board.new_today, { from: todayISO() }),
    metric("Активные диалоги", board.active_dialogs, { status: "" }),
    metric("Ждём клиента", byStatus.waiting_for_client, { status: "waiting_for_client" }),
    metric("Нужен консультант", byStatus.needs_consultant, { status: "needs_consultant" }, "flag"),
    metric("Квалифицированы", byStatus.qualified, { status: "qualified" }),
    metric("Успешно закрыты", byStatus.won, { status: "won" }),
    metric("Закрыты", byStatus.closed, { status: "closed" }),
    metric("Потеряны", byStatus.lost, { status: "lost" }),
    metric("Непрочитанные", board.unread, { unread: "1" }, "flag"),
    metric("Заблокированы", board.blocked, { blocked: "1" }, board.blocked ? "alert" : ""),
    metric("Ведёт ассистент", board.ai_conversations, { mode: "ai" }),
    metric("Ведёт консультант", board.human_conversations, { mode: "human" }));

  const total = Math.max(1, Object.values(byStatus).reduce((a, b) => a + b, 0));
  const bars = el("div", { class: "bars" });
  for (const status of state.statuses) {
    const n = byStatus[status.code] || 0;
    if (!n) continue;
    bars.append(el("div", { class: "bar-row" },
      el("div", { class: "truncate" }, status.label),
      el("div", { class: "bar-track" }, el("div", { class: "bar-fill", style: `width:${Math.round((n / total) * 100)}%` })),
      el("div", { class: "n" }, String(n))));
  }

  const usage = board.ai_usage || {};
  return el("div", {},
    Topbar("Дашборд", el("button", { class: "btn sm", onclick: () => render() }, "Обновить")),
    el("div", { class: "content" },
      metrics,
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Воронка"),
        el("div", { class: "panel-body" }, bars.childNodes.length ? bars : el("div", { class: "empty" }, "Пока нет данных"))),
      el("div", { style: "height:14px" }),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Сегодня"),
        el("div", { class: "panel-body" },
          el("dl", { class: "kv" },
            el("dt", {}, "Сообщений"), el("dd", {}, String(board.messages_today || 0)),
            el("dt", {}, "От клиентов"), el("dd", {}, String(board.inbound_today || 0)),
            el("dt", {}, "Запросов к ИИ"), el("dd", {}, String(usage.calls || 0)),
            el("dt", {}, "Токенов вход"), el("dd", {}, String(usage.input_tokens || 0)),
            el("dt", {}, "Токенов выход"), el("dd", {}, String(usage.output_tokens || 0)),
            el("dt", {}, "Напоминаний"), el("dd", {}, String(board.scheduled_follow_ups || 0)))))));
}

const todayISO = () => new Date().toISOString().slice(0, 10);

function openClients(filter = {}) {
  Object.assign(state.filters, { q: "", status: "", service: "", language: "", mode: "", assigned: "", blocked: "", from: "", to: "", unread: "", offset: 0 }, filter);
  navigate("clients");
}

/* ----------------------------------------------------------- client list */

async function ClientsView() {
  const f = state.filters;
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(f)) if (v !== "" && v !== undefined && v !== null) params.set(k, v);

  const data = await get(`/clients?${params}`);
  state.clients = { items: data.items, total: data.total, loading: false };

  const onFilter = (key) => (e) => { state.filters[key] = e.target.value; state.filters.offset = 0; render(); };

  const search = el("input", { type: "search", placeholder: "Имя, телефон, тег…", value: f.q });
  let searchTimer;
  search.addEventListener("input", (e) => {
    clearTimeout(searchTimer);
    const value = e.target.value;
    searchTimer = setTimeout(() => { state.filters.q = value; state.filters.offset = 0; render(); }, 280);
  });

  const filters = el("div", { class: "filters" },
    el("div", { class: "field" }, el("label", {}, "Поиск"), search),
    field("Статус", selectOf([["", "Все"], ...state.statuses.map((s) => [s.code, s.label])], f.status, onFilter("status"))),
    field("Услуга", selectOf([["", "Все"], ...state.services.map((s) => [s.code, s.name_ru])], f.service, onFilter("service"))),
    field("Язык", selectOf([["", "Все"], ["ru", "Русский"], ["kk", "Қазақша"], ["en", "English"]], f.language, onFilter("language"))),
    field("Режим", selectOf([["", "Все"], ["ai", "Ассистент"], ["human", "Консультант"], ["paused", "Пауза"]], f.mode, onFilter("mode"))),
    field("Консультант", selectOf([["", "Все"], ["0", "Без консультанта"], ...state.consultants.map((c) => [String(c.id), c.name || c.email])], f.assigned, onFilter("assigned"))),
    field("Блокировка", selectOf([["", "Все"], ["0", "Активные"], ["1", "Заблокированы"]], f.blocked, onFilter("blocked"))),
    field("С даты", el("input", { type: "date", value: f.from, onchange: onFilter("from") })),
    field("По дату", el("input", { type: "date", value: f.to, onchange: onFilter("to") })),
    el("button", { class: "btn sm", onclick: () => openClients({}) }, "Сбросить"));

  const head = (label, key, extra = "") => el("th", {
    class: `sortable ${extra}`,
    onclick: () => { state.filters.dir = (f.sort === key && f.dir === "desc") ? "asc" : "desc"; state.filters.sort = key; render(); },
  }, label + (f.sort === key ? (f.dir === "desc" ? " ↓" : " ↑") : ""));

  const body = el("tbody");
  for (const c of data.items) {
    body.append(el("tr", { class: c.unread ? "unread" : "", onclick: () => navigate("client", { id: c.id }) },
      el("td", {},
        el("div", { class: "cell-main" }, c.name || "Без имени"),
        el("div", { class: "cell-sub truncate" }, c.last_message || "—")),
      el("td", { class: "nowrap cell-sub" }, c.phone),
      el("td", {}, el("span", { class: "chip" }, (c.language || "ru").toUpperCase())),
      el("td", { class: "truncate" }, c.service ? c.service_name : "—"),
      el("td", {}, statusChip(c.status, c.status_label)),
      el("td", {}, modeChip(c.mode, c.blocked)),
      el("td", { class: "cell-sub truncate" }, c.assigned_name || "—"),
      el("td", { class: "nowrap cell-sub" }, relative(c.last_message_at || c.last_activity)),
      el("td", { class: "nowrap cell-sub" }, c.next_follow_up ? fmtTime(c.next_follow_up) : "—"),
      el("td", { class: "num" }, c.unread ? el("span", { class: "badge" }, String(c.unread)) : ""),
      el("td", { class: "nowrap" }, el("button", {
        class: "btn sm ghost",
        onclick: (e) => { e.stopPropagation(); navigate("client", { id: c.id }); },
      }, "Открыть"))));
  }

  const from = data.total === 0 ? 0 : f.offset + 1;
  const to = Math.min(f.offset + f.limit, data.total);

  return el("div", {},
    Topbar("Клиенты",
      el("span", { class: "hint" }, `${data.total} всего`),
      el("button", { class: "btn sm", onclick: () => render() }, "Обновить")),
    el("div", { class: "content" },
      filters,
      el("div", { class: "panel" },
        el("div", { class: "table-wrap" },
          el("table", {},
            el("thead", {}, el("tr", {},
              head("Клиент", "name"), el("th", {}, "Телефон"), el("th", {}, "Язык"),
              el("th", {}, "Услуга"), head("Статус", "status"), el("th", {}, "Режим"),
              el("th", {}, "Консультант"), head("Активность", "last_activity"),
              head("Напоминание", "follow_up"), head("Непроч.", "unread", "num"), el("th", {}, ""))),
            body)),
        data.items.length === 0 ? el("div", { class: "empty" }, "Клиенты не найдены") : null,
        el("div", { class: "pager" },
          el("span", {}, `${from}–${to} из ${data.total}`),
          el("div", { class: "grow" }),
          el("button", { class: "btn sm", disabled: f.offset === 0, onclick: () => { state.filters.offset = Math.max(0, f.offset - f.limit); render(); } }, "Назад"),
          el("button", { class: "btn sm", disabled: to >= data.total, onclick: () => { state.filters.offset = f.offset + f.limit; render(); } }, "Вперёд")))));
}

const field = (label, control) => el("div", { class: "field" }, el("label", {}, label), control);

function selectOf(options, value, onchange) {
  const sel = el("select", { onchange });
  for (const [v, label] of options) sel.append(el("option", { value: v, selected: String(value) === String(v) }, label));
  return sel;
}

function statusChip(code, label) {
  const tone = { won: "green", qualified: "green", lost: "red", blocked: "red", closed: "", needs_consultant: "amber", consultant_processing: "violet", waiting_for_client: "amber", new: "blue", ai_processing: "blue" }[code] || "";
  return el("span", { class: `chip ${tone}` }, label || code);
}

function modeChip(mode, blocked) {
  if (blocked) return el("span", { class: "chip red" }, "Блок");
  const map = { ai: ["blue", "Ассистент"], human: ["green", "Консультант"], paused: ["amber", "Пауза"] };
  const [tone, label] = map[mode] || ["", mode];
  return el("span", { class: `chip ${tone}` }, label);
}

function ModeText(mode) {
  return ({ ai: "Ассистент", human: "Консультант", paused: "Пауза" }[mode] || mode || "—");
}

function serviceStatusLabel(status, label) {
  return label || state.statuses.find((s) => s.code === status)?.label || status || "—";
}

/* --------------------------------------------------------- client detail */

async function ClientView(id) {
  const [client, msgs, notes] = await Promise.all([
    get(`/clients/${id}`),
    get(`/clients/${id}/messages?limit=60`),
    get(`/clients/${id}/notes`),
  ]);
  state.client = client;
  state.messages = msgs.items;
  state.hasMoreMessages = msgs.has_more;
  state.notes = notes.items;

  if (client.unread > 0) {
    post(`/clients/${id}/read`).catch(() => {});
    state.client.unread = 0;
  }
  startStream(id);

  return el("div", {},
    Topbar(client.name || client.phone,
      statusChip(client.status, client.status_label),
      modeChip(client.mode, client.blocked),
      el("button", { class: "btn sm", onclick: () => navigate("clients") }, "← К списку")),
    el("div", { class: "content" },
      el("div", { class: "workspace" },
        el("div", { class: "col col-sticky" }, ClientCard(client)),
        el("div", { class: "col" }, ChatPanel(client)),
        el("div", { class: "col crm col-sticky" }, CrmPanel(client)))));
}

function ClientCard(c) {
  const busy = (name) => state.busy.has(name);
  const act = async (name, fn) => {
    if (busy(name)) return;
    state.busy.add(name);
    try { await fn(); toast("Готово", "ok"); await reloadClient(); }
    catch (err) { toast(err.message, "err"); }
    finally { state.busy.delete(name); }
  };

  const actions = el("div", { class: "actions" });
  if (c.mode !== "human") {
    actions.append(el("button", { class: "btn primary sm", disabled: c.blocked, onclick: () => act("takeover", () => post(`/clients/${c.id}/takeover`)) }, "Взять диалог"));
  } else {
    actions.append(el("button", { class: "btn sm", onclick: () => act("resume", () => post(`/clients/${c.id}/resume-ai`)) }, "Вернуть ассистенту"));
  }
  if (c.mode !== "paused") {
    actions.append(el("button", { class: "btn sm", onclick: () => act("pause", () => post(`/clients/${c.id}/pause-ai`)) }, "Пауза ИИ"));
  }
  actions.append(c.blocked
    ? el("button", { class: "btn sm", onclick: () => act("unblock", () => post(`/clients/${c.id}/unblock`)) }, "Разблокировать")
    : el("button", {
        class: "btn danger sm",
        onclick: async () => {
          const reason = await confirmDialog({
            title: "Заблокировать клиента?",
            body: "Автоответы и напоминания остановятся. История сохранится.",
            confirmLabel: "Заблокировать", danger: true,
            field: { label: "Причина (необязательно)", placeholder: "спам" },
          });
          if (reason === null) return;
          await act("block", () => post(`/clients/${c.id}/block`, { reason }));
        },
      }, "Заблокировать"));

  const statusSelect = selectOf(
    state.statuses.filter((s) => s.code !== "blocked").map((s) => [s.code, s.label]),
    c.status,
    async (e) => {
      const next = e.target.value;
      const terminal = state.statuses.find((s) => s.code === next)?.terminal;
      let reason = "";
      if (terminal) {
        reason = await confirmDialog({
          title: "Закрыть лид?",
          body: "Все запланированные напоминания будут отменены.",
          confirmLabel: "Подтвердить",
          field: { label: "Причина", placeholder: "например: клиент выбрал другого юриста" },
        });
        if (reason === null) { render(); return; }
      }
      await act("status", () => post(`/clients/${c.id}/status`, { status: next, reason }));
    });

  const assignSelect = selectOf(
    [["0", "— не назначен —"], ...state.consultants.map((a) => [String(a.id), a.name || a.email])],
    String(c.assigned_id || 0),
    (e) => act("assign", () => post(`/clients/${c.id}/assign`, { consultant_id: Number(e.target.value) })));

  const langSelect = selectOf([["ru", "Русский"], ["kk", "Қазақша"], ["en", "English"]], c.language,
    (e) => act("lang", () => patch(`/clients/${c.id}`, { language: e.target.value })));

  const serviceSelect = selectOf([["", "— не определена —"], ...state.services.map((s) => [s.code, s.name_ru])], c.service || "",
    (e) => act("service", () => patch(`/clients/${c.id}`, { service: e.target.value })));

  return el("div", { class: "row-gap" },
    el("div", { class: "panel" },
      el("div", { class: "panel-head" }, "Клиент"),
      el("div", { class: "panel-body row-gap" },
        el("dl", { class: "kv" },
          el("dt", {}, "Телефон"), el("dd", {}, c.phone || "—"),
          el("dt", {}, "WhatsApp"), el("dd", { class: "hint" }, c.whatsapp_id || "—"),
          el("dt", {}, "Первый контакт"), el("dd", {}, fmtDate(c.first_seen_at)),
          el("dt", {}, "Последнее входящее"), el("dd", {}, fmtTime(c.last_inbound_at)),
          el("dt", {}, "Последнее исходящее"), el("dd", {}, fmtTime(c.last_outbound_at)),
          el("dt", {}, "Оценка лида"), el("dd", {}, (c.lead_score ?? 0).toFixed(2))),
        el("div", {}, el("div", { class: "section-title" }, "Статус"), statusSelect),
        el("div", {}, el("div", { class: "section-title" }, "Консультант"), assignSelect),
        el("div", {}, el("div", { class: "section-title" }, `Язык${c.language_locked ? " (задан вручную)" : ""}`), langSelect),
        el("div", {}, el("div", { class: "section-title" }, "Услуга"), serviceSelect),
        el("div", {}, el("div", { class: "section-title" }, "Теги"), TagEditor(c)),
        c.blocked && c.block_reason ? el("div", { class: "error-box" }, `Заблокирован: ${c.block_reason}`) : null,
        actions)));
}

function TagEditor(c) {
  const input = el("input", { type: "text", placeholder: "тег и Enter", });
  input.addEventListener("keydown", async (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    const value = input.value.trim();
    if (!value) return;
    const tags = [...(c.tags || []), value];
    try { await patch(`/clients/${c.id}`, { tags }); await reloadClient(); }
    catch (err) { toast(err.message, "err"); }
  });

  const chips = el("div", { class: "inline", style: "flex-wrap:wrap;margin-bottom:6px" });
  for (const tag of c.tags || []) {
    chips.append(el("span", { class: "chip" }, tag, el("button", {
      class: "btn ghost sm", style: "padding:0 3px",
      onclick: async () => {
        try { await patch(`/clients/${c.id}`, { tags: (c.tags || []).filter((t) => t !== tag) }); await reloadClient(); }
        catch (err) { toast(err.message, "err"); }
      },
    }, "×")));
  }
  return el("div", {}, (c.tags || []).length ? chips : null, input);
}

/* ------------------------------------------------------------------- chat */

function ChatPanel(c) {
  const scroll = el("div", { class: "chat-scroll" });
  renderMessages(scroll);

  if (state.hasMoreMessages) {
    scroll.prepend(el("div", { style: "text-align:center;margin-bottom:10px" },
      el("button", {
        class: "btn sm",
        onclick: async (e) => {
          e.target.disabled = true;
          const oldest = state.messages[0]?.id || 0;
          try {
            const older = await get(`/clients/${c.id}/messages?before=${oldest}&limit=60`);
            state.messages = [...older.items, ...state.messages];
            state.hasMoreMessages = older.has_more;
            render();
          } catch (err) { toast(err.message, "err"); }
        },
      }, "Загрузить раньше")));
  }

  const locked = c.mode !== "human";
  const textarea = el("textarea", { placeholder: c.blocked ? "Клиент заблокирован" : "Сообщение клиенту…", disabled: c.blocked });
  const fileInput = el("input", { type: "file", style: "display:none" });
  const attachRow = el("div", {});
  const sendBtn = el("button", { class: "btn primary", disabled: c.blocked }, "Отправить");

  const refreshAttach = () => {
    clear(attachRow);
    if (!state.attachment) return;
    attachRow.append(el("div", { class: "attach-preview" },
      el("span", {}, "📎 ", state.attachment.name),
      el("span", { class: "hint" }, fmtBytes(state.attachment.size)),
      el("div", { class: "grow", style: "flex:1" }),
      el("button", { class: "btn ghost sm", onclick: () => { state.attachment = null; fileInput.value = ""; refreshAttach(); } }, "×")));
  };

  fileInput.addEventListener("change", () => {
    state.attachment = fileInput.files[0] || null;
    refreshAttach();
  });

  const send = async () => {
    const text = textarea.value.trim();
    if (!text && !state.attachment) return;
    sendBtn.disabled = true;
    sendBtn.textContent = "Отправка…";
    try {
      let res;
      if (state.attachment) {
        const form = new FormData();
        form.append("file", state.attachment);
        form.append("text", text);
        if (/^audio\/(ogg|opus)/.test(state.attachment.type)) form.append("voice", "1");
        res = await request(`/clients/${c.id}/messages`, { method: "POST", body: form });
      } else {
        res = await post(`/clients/${c.id}/messages`, { text });
      }
      textarea.value = "";
      state.attachment = null;
      fileInput.value = "";
      if (res.warning) toast(res.warning, "err");
      await reloadClient();
    } catch (err) {
      toast(err.message, "err");
    } finally {
      sendBtn.disabled = false;
      sendBtn.textContent = "Отправить";
    }
  };

  sendBtn.addEventListener("click", send);
  textarea.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send(); }
  });

  const composer = el("div", { class: `composer ${locked && !c.blocked ? "locked" : ""}` },
    locked && !c.blocked
      ? el("div", { class: "composer-note" },
          el("span", {}, "Диалог ведёт ассистент. Отправка сообщения переведёт его на вас."),
          el("button", { class: "btn sm", onclick: async () => { try { await post(`/clients/${c.id}/takeover`); await reloadClient(); } catch (err) { toast(err.message, "err"); } } }, "Взять сейчас"))
      : null,
    attachRow,
    el("div", { class: "composer-row" },
      el("button", { class: "btn", disabled: c.blocked, onclick: () => fileInput.click(), title: "Прикрепить файл" }, "📎"),
      textarea,
      sendBtn),
    fileInput,
    el("div", { class: "hint", style: "margin-top:5px" }, "Ctrl+Enter — отправить. Фото, видео, аудио, голосовые и документы поддерживаются."));

  const panel = el("div", { class: "panel" },
    el("div", { class: "panel-head" }, "Переписка", el("div", { class: "grow" }),
      el("span", { class: "hint" }, `${state.messages.length} сообщений`)),
    el("div", { class: "chat" }, scroll, composer));

  requestAnimationFrame(() => { scroll.scrollTop = scroll.scrollHeight; });
  return panel;
}

function renderMessages(scroll) {
  if (!state.messages.length) {
    scroll.append(el("div", { class: "empty" }, "Переписки пока нет"));
    return;
  }
  let lastDay = "";
  for (const m of state.messages) {
    const day = new Date(m.created_at).toDateString();
    if (day !== lastDay) {
      lastDay = day;
      scroll.append(el("div", { class: "chat-day" }, el("span", {}, fmtDate(m.created_at))));
    }
    scroll.append(MessageBubble(m));
  }
}

const SENDER_LABEL = { client: "Клиент", ai: "Ассистент", consultant: "Консультант", system: "Система" };

function MessageBubble(m) {
  const sender = m.sender_type || m.sender;
  if (sender === "system") {
    return el("div", { class: "msg sys" }, el("div", { class: "bubble" }, m.text));
  }
  const out = m.direction_type ? m.direction_type === "outbound" : m.direction === "outgoing";
  const bubble = el("div", { class: "bubble" });
  bubble.append(el("div", { class: "who" }, SENDER_LABEL[sender] || m.sender_name || sender));

  if (m.media) bubble.append(MediaBlock(m));
  else if (m.media_unavailable) bubble.append(el("div", { class: "hint" }, `[${m.type}] файл недоступен`));

  if (m.text) bubble.append(el("div", { class: "body" }, m.text));

  const meta = el("div", { class: "meta" }, fmtTime(m.created_at));
  if (out && m.delivery === "failed") meta.append(" · ", el("span", { class: "fail" }, "не доставлено"));
  else if (out && m.delivery === "sent") meta.append(" · ✓");
  bubble.append(meta);

  return el("div", { class: `msg ${out ? "out" : "in"} ${sender === "consultant" ? "consultant" : ""}` }, bubble);
}

function MediaBlock(m) {
  const { url, name, mime, size } = m.media;
  if (mime?.startsWith("image/")) return el("img", { src: url, alt: name || "", loading: "lazy" });
  if (mime?.startsWith("video/")) return el("video", { src: url, controls: true, preload: "metadata" });
  if (mime?.startsWith("audio/")) return el("audio", { src: url, controls: true, preload: "metadata" });
  return el("a", { class: "file-link", href: url, target: "_blank", rel: "noopener" },
    "📄 ", name || "файл", el("span", { class: "hint" }, fmtBytes(size)));
}

/* --------------------------------------------------------------- CRM pane */

function CrmPanel(c) {
  const noteInput = el("textarea", { placeholder: "Внутренняя заметка (не уходит клиенту)…" });
  const addNote = async () => {
    const body = noteInput.value.trim();
    if (!body) return;
    try { await post(`/clients/${c.id}/notes`, { body }); noteInput.value = ""; await reloadClient(); }
    catch (err) { toast(err.message, "err"); }
  };

  const notes = el("div", {});
  if (!state.notes.length) notes.append(el("div", { class: "hint" }, "Заметок пока нет"));
  for (const n of state.notes) {
    notes.append(el("div", { class: "note" },
      el("div", { class: "head" },
        el("span", { class: "author" }, n.author || `#${n.author_id}`),
        el("span", {}, fmtTime(n.created_at)),
        el("div", { style: "flex:1" }),
        el("button", {
          class: "btn ghost sm", title: "Удалить",
          onclick: async () => {
            const ok = await confirmDialog({ title: "Удалить заметку?", body: "Действие нельзя отменить.", confirmLabel: "Удалить", danger: true });
            if (!ok) return;
            try { await del(`/notes/${n.id}`); await reloadClient(); } catch (err) { toast(err.message, "err"); }
          },
        }, "×")),
      el("div", { class: "body" }, n.body)));
  }

  const facts = el("ul", { class: "facts" });
  for (const f of c.important_facts || []) facts.append(el("li", {}, f));

  const followUp = c.next_follow_up_job
    ? el("div", { class: "inline" },
        el("span", { class: "chip amber" }, `Этап ${c.next_follow_up_job.stage}`),
        el("span", {}, fmtTime(c.next_follow_up_job.scheduled_at)),
        el("button", {
          class: "btn ghost sm",
          onclick: async () => {
            try { await post(`/follow-ups/${c.next_follow_up_job.id}/cancel`); toast("Напоминание отменено", "ok"); await reloadClient(); }
            catch (err) { toast(err.message, "err"); }
          },
        }, "Отменить"))
    : el("div", { class: "hint" }, "Не запланировано");

  return el("div", { class: "row-gap" },
    el("div", { class: "panel" },
      el("div", { class: "panel-head" }, "Клиент"),
      el("div", { class: "panel-body" },
        el("dl", { class: "kv" },
          el("dt", {}, "Имя"), el("dd", {}, c.name || c.phone || "—"),
          el("dt", {}, "Телефон"), el("dd", {}, c.phone || "—"),
          el("dt", {}, "Статус"), el("dd", {}, serviceStatusLabel(c.status, c.status_label)),
          el("dt", {}, "State"), el("dd", {}, c.current_state || c.qualification_stage || "—"),
          el("dt", {}, "Режим"), el("dd", {}, ModeText(c.mode)),
          el("dt", {}, "Активность"), el("dd", {}, relative(c.last_message_at || c.last_inbound_at || c.last_outbound_at)),
          el("dt", {}, "WhatsApp"), el("dd", {}, c.whatsapp_id || "—"),
          el("dt", {}, "Оператор"), el("dd", {}, c.assigned_name || "—")))),
    el("div", { class: "panel" },
      el("div", { class: "panel-head" }, "Анализ ассистента"),
      el("div", { class: "panel-body row-gap" },
        el("div", {}, el("div", { class: "section-title" }, "Краткое резюме"),
          el("div", {}, c.summary || el("span", { class: "hint" }, "Пока нет"))),
        el("div", {}, el("div", { class: "section-title" }, "Важные факты"),
          (c.important_facts || []).length ? facts : el("div", { class: "hint" }, "Пока нет")),
        el("dl", { class: "kv" },
          el("dt", {}, "Этап"), el("dd", {}, stageLabel(c.qualification_stage)),
          el("dt", {}, "Намерение"), el("dd", {}, c.intent || "—"),
          el("dt", {}, "Уверенность"), el("dd", {}, c.confidence ? `${Math.round(c.confidence * 100)}%` : "—")),
        el("div", {}, el("div", { class: "section-title" }, "Следующий шаг"),
          el("div", {}, c.next_action || el("span", { class: "hint" }, "—"))),
        el("div", {}, el("div", { class: "section-title" }, "Напоминание"), followUp))),
    el("div", { class: "panel" },
      el("div", { class: "panel-head" }, "Внутренние заметки"),
      el("div", { class: "panel-body row-gap" },
        noteInput,
        el("button", { class: "btn sm", onclick: addNote }, "Добавить заметку"),
        notes)));
}

const stageLabel = (stage) => ({
  language_detected: "Язык определён", intent_detected: "Запрос определён", qualifying: "Уточнение",
  qualified: "Квалифицирован", waiting_for_client: "Ждём клиента", consultant_required: "Нужен консультант",
  human_takeover: "Ведёт консультант", converted: "Завершено",
}[stage] || "—");

/* ------------------------------------------------------------ other views */

async function ConversationsView() {
  const data = await get("/clients?status=needs_consultant,consultant_processing&limit=50&sort=last_activity");
  const rows = el("tbody");
  for (const c of data.items) {
    rows.append(el("tr", { onclick: () => navigate("client", { id: c.id }) },
      el("td", {}, el("div", { class: "cell-main" }, c.name || c.phone), el("div", { class: "cell-sub truncate" }, c.last_message || "")),
      el("td", { class: "nowrap cell-sub" }, c.phone),
      el("td", {}, statusChip(c.status, c.status_label)),
      el("td", {}, modeChip(c.mode, c.blocked)),
      el("td", { class: "cell-sub" }, c.assigned_name || "—"),
      el("td", { class: "nowrap cell-sub" }, relative(c.last_message_at))));
  }
  return el("div", {},
    Topbar("Диалоги, требующие консультанта"),
    el("div", { class: "content" },
      el("div", { class: "panel" },
        el("div", { class: "table-wrap" }, el("table", {},
          el("thead", {}, el("tr", {}, el("th", {}, "Клиент"), el("th", {}, "Телефон"), el("th", {}, "Статус"), el("th", {}, "Режим"), el("th", {}, "Консультант"), el("th", {}, "Активность"))),
          rows)),
        data.items.length === 0 ? el("div", { class: "empty" }, "Все диалоги под контролем") : null)));
}

async function FollowUpsView() {
  const statusSel = new URLSearchParams(location.hash.split("?")[1] || "").get("status") || "pending";
  const data = await get(`/follow-ups?status=${statusSel}&limit=100`);
  const rows = el("tbody");
  for (const job of data.items) {
    rows.append(el("tr", {},
      el("td", { onclick: () => navigate("client", { id: job.client_id }) }, el("div", { class: "cell-main" }, job.client_name || job.client_phone)),
      el("td", { class: "nowrap cell-sub" }, job.client_phone),
      el("td", {}, el("span", { class: "chip" }, `Этап ${job.stage}`)),
      el("td", { class: "nowrap" }, fmtTime(job.scheduled_at)),
      el("td", {}, el("span", { class: "chip" }, job.status)),
      el("td", { class: "num cell-sub" }, String(job.attempts)),
      el("td", { class: "cell-sub truncate" }, job.last_error || ""),
      el("td", { class: "nowrap" },
        job.status === "pending"
          ? el("div", { class: "inline" },
              el("button", {
                class: "btn sm", onclick: async () => {
                  try { await post(`/follow-ups/${job.id}/reschedule`, { in_minutes: 60 }); toast("Перенесено на час", "ok"); render(); }
                  catch (err) { toast(err.message, "err"); }
                },
              }, "+1 час"),
              el("button", {
                class: "btn sm danger", onclick: async () => {
                  try { await post(`/follow-ups/${job.id}/cancel`); toast("Отменено", "ok"); render(); }
                  catch (err) { toast(err.message, "err"); }
                },
              }, "Отменить"))
          : null)));
  }

  const filter = selectOf(
    [["pending", "Запланированные"], ["sent", "Отправленные"], ["cancelled", "Отменённые"], ["skipped", "Пропущенные"], ["failed", "Ошибки"]],
    statusSel, (e) => { location.hash = `#/followups?status=${e.target.value}`; });

  return el("div", {},
    Topbar("Напоминания", filter),
    el("div", { class: "content" },
      el("div", { class: "panel" },
        el("div", { class: "table-wrap" }, el("table", {},
          el("thead", {}, el("tr", {}, el("th", {}, "Клиент"), el("th", {}, "Телефон"), el("th", {}, "Этап"), el("th", {}, "Время"), el("th", {}, "Статус"), el("th", { class: "num" }, "Попыток"), el("th", {}, "Примечание"), el("th", {}, ""))),
          rows)),
        data.items.length === 0 ? el("div", { class: "empty" }, "Ничего нет") : null)));
}

async function ConsultantsView() {
  const data = await get("/consultants");
  const isAdmin = state.user?.role === "admin";
  const rows = el("tbody");
  for (const a of data.items) {
    rows.append(el("tr", {},
      el("td", { class: "cell-main" }, a.name || "—"),
      el("td", {}, a.email),
      el("td", {}, el("span", { class: "chip" }, roleLabel(a.role))),
      el("td", {}, a.active ? el("span", { class: "chip green" }, "Активен") : el("span", { class: "chip red" }, "Отключён")),
      el("td", { class: "nowrap cell-sub" }, a.last_login ? fmtTime(a.last_login) : "—"),
      el("td", { class: "nowrap" }, isAdmin
        ? el("button", {
            class: "btn sm", onclick: async () => {
              try { await patch(`/consultants/${a.id}`, { active: !a.active }); toast("Обновлено", "ok"); render(); }
              catch (err) { toast(err.message, "err"); }
            },
          }, a.active ? "Отключить" : "Включить")
        : null)));
  }

  const form = isAdmin ? NewConsultantForm() : null;
  return el("div", {},
    Topbar("Консультанты"),
    el("div", { class: "content row-gap" },
      el("div", { class: "panel" },
        el("div", { class: "table-wrap" }, el("table", {},
          el("thead", {}, el("tr", {}, el("th", {}, "Имя"), el("th", {}, "Email"), el("th", {}, "Роль"), el("th", {}, "Статус"), el("th", {}, "Последний вход"), el("th", {}, ""))),
          rows))),
      form));
}

function NewConsultantForm() {
  const name = el("input", { type: "text", placeholder: "Имя" });
  const email = el("input", { type: "email", placeholder: "email@company.kz" });
  const password = el("input", { type: "password", placeholder: "минимум 10 символов, буква и цифра" });
  const role = selectOf([["consultant", "Консультант"], ["admin", "Администратор"], ["readonly", "Только чтение"]], "consultant", () => {});

  return el("div", { class: "panel" },
    el("div", { class: "panel-head" }, "Новый аккаунт"),
    el("div", { class: "panel-body row-gap" },
      el("div", { class: "filters" }, field("Имя", name), field("Email", email), field("Пароль", password), field("Роль", role)),
      el("button", {
        class: "btn primary", onclick: async () => {
          try {
            await post("/consultants", { name: name.value, email: email.value, password: password.value, role: role.value });
            toast("Аккаунт создан", "ok");
            await loadConsultants();
            render();
          } catch (err) { toast(err.message, "err"); }
        },
      }, "Создать")));
}

async function ServicesView() {
  const data = await get("/services");
  const rows = el("tbody");
  for (const s of data.items) {
    rows.append(el("tr", {},
      el("td", { class: "cell-main" }, s.name_ru),
      el("td", {}, s.name_kk),
      el("td", { class: "cell-sub" }, s.code),
      el("td", { class: "cell-sub truncate" }, s.clarify_ru),
      el("td", {}, s.fixed_price ? el("span", { class: "chip green" }, s.price) : el("span", { class: "chip" }, "по запросу"))));
  }
  return el("div", {},
    Topbar("Услуги"),
    el("div", { class: "content" },
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Каталог услуг", el("div", { class: "grow" }),
          el("span", { class: "hint" }, "Ассистент отвечает только по этим направлениям")),
        el("div", { class: "table-wrap" }, el("table", {},
          el("thead", {}, el("tr", {}, el("th", {}, "Название (RU)"), el("th", {}, "Атауы (KZ)"), el("th", {}, "Код"), el("th", {}, "Уточняющий вопрос"), el("th", {}, "Цена"))),
          rows)))));
}

async function AnalyticsView() {
  const data = await get("/analytics?days=30");
  const max = Math.max(1, ...data.daily.map((d) => d.count));
  const spark = el("div", { class: "spark" });
  for (const d of data.daily) {
    spark.append(el("div", { style: `height:${Math.max(3, (d.count / max) * 100)}%`, title: `${d.day}: ${d.count}` }));
  }

  const services = el("div", { class: "bars" });
  const maxService = Math.max(1, ...data.services.map((s) => s.count));
  for (const s of data.services) {
    services.append(el("div", { class: "bar-row" },
      el("div", { class: "truncate" }, s.name),
      el("div", { class: "bar-track" }, el("div", { class: "bar-fill", style: `width:${(s.count / maxService) * 100}%` })),
      el("div", { class: "n" }, String(s.count))));
  }

  const usage = data.ai_usage || {};
  return el("div", {},
    Topbar("Аналитика"),
    el("div", { class: "content row-gap" },
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Новые клиенты за 30 дней"),
        el("div", { class: "panel-body" }, data.daily.length ? spark : el("div", { class: "empty" }, "Нет данных"))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Запросы по услугам"),
        el("div", { class: "panel-body" }, data.services.length ? services : el("div", { class: "empty" }, "Нет данных"))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Расход модели за 30 дней"),
        el("div", { class: "panel-body" },
          el("dl", { class: "kv" },
            el("dt", {}, "Запросов"), el("dd", {}, String(usage.calls || 0)),
            el("dt", {}, "Входных токенов"), el("dd", {}, String(usage.input_tokens || 0)),
            el("dt", {}, "Выходных токенов"), el("dd", {}, String(usage.output_tokens || 0)))))));
}

function ExportsView() {
  const lang = selectOf([["kk", "Қазақша"], ["ru", "Русский"], ["en", "English"]], "kk", () => {});
  const format = selectOf([["xlsx", "Excel (.xlsx)"], ["csv", "CSV"]], "xlsx", () => {});
  const status = selectOf([["", "Все статусы"], ...state.statuses.map((s) => [s.code, s.label])], "", () => {});

  const download = () => {
    const params = new URLSearchParams({ format: format.value, lang: lang.value });
    if (status.value) params.set("status", status.value);
    // The export endpoint is authenticated by the same session cookie.
    window.location.href = `${API}/export?${params}`;
  };

  return Promise.resolve(el("div", {},
    Topbar("Экспорт"),
    el("div", { class: "content" },
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Выгрузка клиентов"),
        el("div", { class: "panel-body row-gap" },
          el("div", { class: "filters" }, field("Формат", format), field("Язык заголовков", lang), field("Статус", status)),
          el("button", { class: "btn primary", onclick: download }, "Скачать"),
          el("div", { class: "hint" },
            "Выгружаются: клиент, телефон, язык, услуга, статус, консультант, первый и последний контакт, этап, резюме, следующий шаг, напоминание, теги, результат. ",
            "Служебные поля (хэши паролей, токены, ключи) в экспорт не попадают."))))));
}

async function SettingsView() {
  const s = await get("/settings");
  state.settings = s;
  const current = el("input", { type: "password", placeholder: "текущий пароль" });
  const next = el("input", { type: "password", placeholder: "новый пароль" });
  const botEnabled = s.whatsapp?.bot_enabled !== false;
  const botToggle = el("button", {
    class: `bot-switch settings ${botEnabled ? "on" : "off"}`,
    disabled: state.busy.has("whatsapp-bot"),
    onclick: () => setBotEnabled(!botEnabled),
  }, el("span", {}, botEnabled ? "ON" : "OFF"));

  return el("div", {},
    Topbar("Настройки"),
    el("div", { class: "content row-gap" },
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "WhatsApp"),
        el("div", { class: "panel-body row-gap" },
          el("div", { class: "settings-bot-row" },
            el("div", {},
              el("div", { class: "section-title" }, "Автоответы ассистента"),
              el("div", { class: "hint" }, botEnabled ? "Включены" : "Выключены")),
            botToggle),
          el("dl", { class: "kv" },
            el("dt", {}, "Подключение"), el("dd", {}, botConnectionLabel(s.whatsapp?.connection_status))))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Ассистент"),
        el("div", { class: "panel-body" },
          el("dl", { class: "kv" },
            el("dt", {}, "Модель ответов"), el("dd", {}, s.ai.reply_model),
            el("dt", {}, "Модель анализа"), el("dd", {}, s.ai.classifier_model),
            el("dt", {}, "Лимит токенов"), el("dd", {}, String(s.ai.max_output_tokens)),
            el("dt", {}, "Окно истории"), el("dd", {}, `${s.ai.context_messages} сообщений`),
            el("dt", {}, "Ответы модели"), el("dd", {}, s.ai.agent_replies ? "включены" : "шаблоны"),
            el("dt", {}, "Порог уверенности"), el("dd", {}, String(s.ai.min_confidence)),
            el("dt", {}, "Dry run"), el("dd", {}, s.ai.dry_run ? "да" : "нет")))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Напоминания"),
        el("div", { class: "panel-body" },
          el("dl", { class: "kv" },
            el("dt", {}, "Включены"), el("dd", {}, s.follow_up.enabled ? "да" : "нет"),
            el("dt", {}, "Интервалы"), el("dd", {}, (s.follow_up.delays || []).join(" → ") || "—"),
            el("dt", {}, "Макс. попыток"), el("dd", {}, String(s.follow_up.max_attempts)),
            el("dt", {}, "Рабочие часы"), el("dd", {}, s.follow_up.business_hours_only ? `${s.follow_up.business_start}:00–${s.follow_up.business_end}:00 (${s.follow_up.timezone})` : "круглосуточно")),
          el("div", { class: "hint", style: "margin-top:10px" }, "Интервалы задаются переменными окружения FOLLOWUP_DELAYS на сервере."))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Смена пароля"),
        el("div", { class: "panel-body row-gap" },
          el("div", { class: "filters" }, field("Текущий", current), field("Новый", next)),
          el("button", {
            class: "btn primary", onclick: async () => {
              try {
                await post("/auth/password", { current_password: current.value, new_password: next.value });
                toast("Пароль изменён, войдите заново", "ok");
                state.user = null; render();
              } catch (err) { toast(err.message, "err"); }
            },
          }, "Изменить пароль"))),
      el("div", { class: "panel" },
        el("div", { class: "panel-head" }, "Безопасность"),
        el("div", { class: "panel-body" }, el("div", { class: "hint" }, s.note)))));
}

/* ------------------------------------------------------------------ live */

function startStream(clientID) {
  stopStream();
  try {
    const url = clientID ? `${API}/events?client_id=${clientID}` : `${API}/events`;
    const source = new EventSource(url, { withCredentials: true });
    source.addEventListener("client.changed", () => {
      if (state.route.name === "client" && Number(state.route.params.id) === Number(clientID)) {
        reloadMessages().catch(() => {});
      }
    });
    source.onerror = () => { /* the browser reconnects on its own */ };
    state.stream = source;
  } catch { state.stream = null; }
}

function stopStream() {
  if (state.stream) { state.stream.close(); state.stream = null; }
}

async function reloadMessages() {
  const id = state.client?.id;
  if (!id) return;
  const after = state.messages.length ? state.messages[state.messages.length - 1].id : 0;
  const fresh = await get(`/clients/${id}/messages?after=${after}`);
  if (!fresh.items.length) return;
  state.messages = [...state.messages, ...fresh.items];
  const client = await get(`/clients/${id}`);
  state.client = client;
  post(`/clients/${id}/read`).catch(() => {});
  render();
}

async function reloadClient() {
  if (!state.client) return;
  await render();
}

/* --------------------------------------------------------------- routing */

function parseHash() {
  const raw = (location.hash || "#/dashboard").slice(2);
  const [path] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  if (!parts.length) return { name: "dashboard", params: {} };
  if (parts[0] === "clients" && parts[1]) return { name: "client", params: { id: parts[1] } };
  return { name: parts[0], params: {} };
}

function navigate(name, params = {}) {
  location.hash = name === "client" ? `#/clients/${params.id}` : `#/${name}`;
}

const VIEWS = {
  dashboard: DashboardView,
  clients: ClientsView,
  client: (params) => ClientView(params.id),
  conversations: ConversationsView,
  followups: FollowUpsView,
  consultants: ConsultantsView,
  services: ServicesView,
  analytics: AnalyticsView,
  exports: ExportsView,
  settings: SettingsView,
};

/* ------------------------------------------------------------------ boot */

async function loadConsultants() {
  try { state.consultants = (await get("/consultants")).items || []; } catch { state.consultants = []; }
}

async function bootstrap() {
  const [dash, services, settings] = await Promise.allSettled([get("/dashboard"), get("/services"), get("/settings")]);
  if (dash.status === "fulfilled") {
    state.statuses = dash.value.statuses || [];
    state.counts.unread = dash.value.unread || 0;
    state.counts.needsConsultant = dash.value.by_status?.needs_consultant || 0;
    state.counts.followUps = dash.value.scheduled_follow_ups || 0;
  }
  if (services.status === "fulfilled") state.services = services.value.items || [];
  if (settings.status === "fulfilled") state.settings = settings.value;
  await loadConsultants();
}

const root = document.getElementById("app");

async function render() {
  if (!state.user) {
    stopStream();
    root.className = "";
    clear(root).append(LoginView(), toastHost);
    return;
  }

  state.route = parseHash();
  if (state.route.name !== "client") { stopStream(); state.client = null; }

  const main = el("main", { class: "main" });
  root.className = "";
  clear(root).append(el("div", { class: "shell" }, Sidebar(), main), toastHost);
  main.append(el("div", { class: "content" }, el("div", { class: "skeleton", style: "width:180px" })));

  const view = VIEWS[state.route.name] || DashboardView;
  try {
    const node = await view(state.route.params);
    clear(main).append(node);
    // The sidebar counters may have moved while the view was loading.
    root.querySelector(".sidebar").replaceWith(Sidebar());
  } catch (err) {
    if (err instanceof HttpError && err.status === 401) return;
    clear(main).append(Topbar("Ошибка"), el("div", { class: "content" },
      el("div", { class: "error-box" }, err.message),
      el("button", { class: "btn", onclick: () => render() }, "Повторить")));
  }
}

window.addEventListener("hashchange", () => { render(); });

(async function start() {
  try {
    const me = await get("/auth/me");
    state.user = me.user;
    state.csrf = me.csrf_token;
    state.version = me.version || "";
    await bootstrap();
  } catch {
    state.user = null;
  }
  state.booted = true;
  await render();
})();
