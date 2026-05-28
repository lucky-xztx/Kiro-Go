/* Kiro-Go public homepage script. */
(function () {
  "use strict";

  var BASE = window.location.origin;
  var currentUser = null;

  function $(id) {
    return document.getElementById(id);
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s).replace(/[<>&"']/g, function (c) {
      return (
        { "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c] || c
      );
    });
  }

  function showToast(text) {
    var t = $("toast");
    if (!t) return;
    t.textContent = text;
    t.classList.add("show");
    setTimeout(function () {
      t.classList.remove("show");
    }, 1800);
  }

  function safeCopy(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    try {
      document.execCommand("copy");
    } catch (e) {
      /* noop */
    }
    document.body.removeChild(ta);
    return Promise.resolve();
  }

  function copyValue(el) {
    var text = (el.textContent || "").trim();
    if (!text || text === "-") return;
    safeCopy(text).then(function () {
      showToast("已复制：" + text);
    });
  }

  function formatUptime(sec) {
    if (sec == null || isNaN(sec)) return "-";
    sec = Math.max(0, Math.floor(sec));
    if (sec < 60) return sec + "s";
    if (sec < 3600) return Math.floor(sec / 60) + "m";
    if (sec < 86400) {
      var h = Math.floor(sec / 3600);
      var m = Math.floor((sec % 3600) / 60);
      return m ? h + "h " + m + "m" : h + "h";
    }
    var d = Math.floor(sec / 86400);
    var h2 = Math.floor((sec % 86400) / 3600);
    return h2 ? d + "d " + h2 + "h" : d + "d";
  }

  function formatTokens(n) {
    if (n == null || isNaN(n)) return "-";
    n = Number(n);
    if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
    return String(n);
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return "-";
    return Number(n).toLocaleString();
  }

  function formatTime(ts) {
    if (!ts) return "-";
    var d = new Date(typeof ts === "number" ? ts * 1000 : ts);
    if (isNaN(d.getTime())) return "-";
    var pad = function (n) { return n < 10 ? "0" + n : n; };
    return (
      d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
      " " + pad(d.getHours()) + ":" + pad(d.getMinutes())
    );
  }

  function bindCopy() {
    document.querySelectorAll("[data-copy]").forEach(function (el) {
      el.addEventListener("click", function () {
        copyValue(el);
      });
    });
  }

  function setEndpoints() {
    $("claudeUrl").textContent = BASE + "/v1/messages";
    $("openaiUrl").textContent = BASE + "/v1/chat/completions";
    $("modelsUrl").textContent = BASE + "/v1/models";
  }

  function loadStatus() {
    fetch("/api/public/stats")
      .then(function (r) {
        return r.ok ? r.json() : Promise.reject(r.status);
      })
      .then(function (d) {
        if (d.name) {
          $("serverName").textContent = d.name;
          document.title = d.name;
        }
        $("uptime").textContent = formatUptime(d.uptime);
        $("modelCount").textContent =
          d.modelCount != null ? d.modelCount : "-";
        var rate = d.successRate;
        $("successRate").textContent =
          rate == null || isNaN(rate) ? "-" : rate + "%";
        $("totalCalls").textContent = formatNumber(d.totalRequests);
        $("totalTokens").textContent = formatTokens(d.totalTokens);
        $("accountsCount").textContent =
          d.accounts != null ? d.accounts : "-";
        if (d.version) {
          $("versionTag").textContent = "v" + d.version;
        }
      })
      .catch(function () {
        ["uptime", "modelCount", "successRate", "totalCalls", "totalTokens", "accountsCount"].forEach(
          function (id) {
            var el = $(id);
            if (el && el.textContent === "-") el.textContent = "-";
          }
        );
      });
  }

  function loadModels() {
    var listEl = $("modelsList");
    fetch("/v1/models")
      .then(function (r) {
        return r.ok ? r.json() : Promise.reject(r.status);
      })
      .then(function (data) {
        var models = (data && data.data) || [];
        if (!models.length) {
          listEl.innerHTML = '<span class="no-models">暂无可用模型</span>';
          return;
        }
        var ids = [];
        var seen = {};
        models.forEach(function (m) {
          var id = m && m.id;
          if (!id || seen[id]) return;
          seen[id] = true;
          ids.push(id);
        });
        listEl.innerHTML = ids
          .map(function (id) {
            var safe = escapeHTML(id);
            return '<span class="model-tag" data-model="' + safe + '">' + safe + "</span>";
          })
          .join("");
        listEl.querySelectorAll(".model-tag").forEach(function (tag) {
          tag.addEventListener("click", function () {
            var name = tag.getAttribute("data-model");
            safeCopy(name).then(function () {
              showToast("已复制：" + name);
            });
          });
        });
      })
      .catch(function () {
        listEl.innerHTML = '<span class="no-models">加载失败</span>';
      });
  }

  /* ---------------------- Auth ---------------------- */

  function api(path, opts) {
    opts = opts || {};
    opts.credentials = "same-origin";
    opts.headers = opts.headers || {};
    if (opts.body && !opts.headers["Content-Type"]) {
      opts.headers["Content-Type"] = "application/json";
    }
    return fetch(path, opts).then(function (r) {
      return r.json().then(function (data) {
        if (!r.ok) {
          var msg = (data && data.error && data.error.message) || ("HTTP " + r.status);
          var err = new Error(msg);
          err.status = r.status;
          err.data = data;
          throw err;
        }
        return data;
      }, function () {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return {};
      });
    });
  }

  function renderTopBar() {
    var box = $("authActions");
    if (!box) return;
    if (currentUser) {
      var roleLabel = currentUser.role === "admin" ? "ADMIN" : "USER";
      var roleClass = currentUser.role === "admin" ? "" : "user";
      box.innerHTML =
        '<span class="user-chip">' +
          '<span class="role-badge ' + roleClass + '">' + roleLabel + '</span>' +
          escapeHTML(currentUser.username) +
        '</span>' +
        '<button type="button" class="btn-ghost" id="logoutBtn">退出</button>';
      var btn = $("logoutBtn");
      if (btn) btn.addEventListener("click", doLogout);
    } else {
      box.innerHTML =
        '<button type="button" class="btn-ghost" id="loginBtn">登录</button>' +
        '<button type="button" class="btn-pill" id="registerBtn">注册</button>';
      $("loginBtn").addEventListener("click", function () { openAuth("login"); });
      $("registerBtn").addEventListener("click", function () { openAuth("register"); });
    }
    var entry = $("adminEntry");
    if (entry) {
      if (currentUser && currentUser.role === "admin") entry.removeAttribute("hidden");
      else entry.setAttribute("hidden", "");
    }
    var hint = $("apiKeyHint");
    if (hint) {
      if (currentUser) {
        hint.textContent = "在下方「我的 API Key」中生成专属 Key";
      } else {
        hint.textContent = "登录后即可生成专属 API Key";
      }
    }
    var card = $("myKeysCard");
    if (card) {
      if (currentUser) {
        card.removeAttribute("hidden");
        loadMyKeys();
      } else {
        card.setAttribute("hidden", "");
      }
    }
  }

  function loadMe() {
    return api("/api/me").then(function (d) {
      currentUser = (d && d.user) || null;
      renderTopBar();
    }).catch(function () {
      currentUser = null;
      renderTopBar();
    });
  }

  function doLogout() {
    api("/api/logout", { method: "POST" }).then(function () {
      currentUser = null;
      renderTopBar();
      showToast("已退出登录");
    }).catch(function () {
      showToast("退出失败");
    });
  }

  /* ---------------------- Auth modal ---------------------- */

  function openAuth(mode) {
    var overlay = $("authOverlay");
    if (!overlay) return;
    overlay.removeAttribute("hidden");
    setAuthMode(mode || "login");
    var form = $("authForm");
    if (form) form.reset();
    var err = $("authError");
    if (err) err.textContent = "";
    setTimeout(function () {
      var input = form && form.querySelector('input[name="username"]');
      if (input) input.focus();
    }, 30);
  }

  function closeAuth() {
    var overlay = $("authOverlay");
    if (overlay) overlay.setAttribute("hidden", "");
  }

  function setAuthMode(mode) {
    document.querySelectorAll(".auth-tab").forEach(function (tab) {
      if (tab.getAttribute("data-mode") === mode) tab.classList.add("is-active");
      else tab.classList.remove("is-active");
    });
    var form = $("authForm");
    if (!form) return;
    form.setAttribute("data-mode", mode);
    form.querySelectorAll("[data-only]").forEach(function (el) {
      if (el.getAttribute("data-only") === mode) {
        el.removeAttribute("hidden");
        var inp = el.querySelector("input");
        if (inp) inp.disabled = false;
      } else {
        el.setAttribute("hidden", "");
        var inp2 = el.querySelector("input");
        if (inp2) inp2.disabled = true;
      }
    });
    var submit = $("authSubmit");
    if (submit) submit.textContent = mode === "register" ? "注册" : "登录";
  }

  function bindAuthModal() {
    var overlay = $("authOverlay");
    if (!overlay) return;
    document.querySelectorAll(".auth-tab").forEach(function (tab) {
      tab.addEventListener("click", function () {
        setAuthMode(tab.getAttribute("data-mode"));
        var err = $("authError");
        if (err) err.textContent = "";
      });
    });
    var closeBtn = $("authClose");
    if (closeBtn) closeBtn.addEventListener("click", closeAuth);
    overlay.addEventListener("click", function (e) {
      if (e.target === overlay) closeAuth();
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && !overlay.hasAttribute("hidden")) closeAuth();
    });
    var form = $("authForm");
    if (!form) return;
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var mode = form.getAttribute("data-mode") || "login";
      var fd = new FormData(form);
      var username = (fd.get("username") || "").toString().trim();
      var password = (fd.get("password") || "").toString();
      var email = (fd.get("email") || "").toString().trim();
      var err = $("authError");
      if (err) err.textContent = "";
      if (!username || !password) {
        if (err) err.textContent = "请填写用户名和密码";
        return;
      }
      var submit = $("authSubmit");
      if (submit) submit.disabled = true;
      var path = mode === "register" ? "/api/register" : "/api/login";
      var body = mode === "register"
        ? { username: username, password: password, email: email }
        : { username: username, password: password };
      api(path, { method: "POST", body: JSON.stringify(body) })
        .then(function (d) {
          currentUser = (d && d.user) || null;
          renderTopBar();
          closeAuth();
          showToast(mode === "register" ? "注册成功" : "欢迎回来");
        })
        .catch(function (e) {
          if (err) err.textContent = e.message || "操作失败";
        })
        .then(function () {
          if (submit) submit.disabled = false;
        });
    });
  }

  /* ---------------------- Keys ---------------------- */

  function loadMyKeys() {
    var box = $("keysList");
    if (!box) return;
    box.innerHTML = '<p class="muted-line">加载中...</p>';
    api("/api/me/keys").then(function (d) {
      var keys = (d && d.keys) || [];
      renderKeys(keys);
    }).catch(function (e) {
      box.innerHTML = '<p class="muted-line">加载失败：' + escapeHTML(e.message || "") + "</p>";
    });
  }

  function renderKeys(keys) {
    var box = $("keysList");
    if (!box) return;
    if (!keys.length) {
      box.innerHTML = '<p class="muted-line">还没有 Key，点击右上角生成。</p>';
      return;
    }
    box.innerHTML = keys.map(function (k) {
      var status = k.enabled
        ? '<span class="pill">启用</span>'
        : '<span class="pill off">已禁用</span>';
      var tokens = formatTokens(k.tokensUsed || 0);
      var tlimit = k.tokenLimit ? "/" + formatTokens(k.tokenLimit) : "";
      var credits = (k.creditsUsed || 0).toFixed(2);
      var climit = k.creditLimit ? "/" + Number(k.creditLimit).toFixed(2) : "";
      var last = k.lastUsedAt ? formatTime(k.lastUsedAt) : "未使用";
      return (
        '<div class="key-item" data-id="' + escapeHTML(k.id) + '">' +
          '<div class="key-item-main">' +
            '<div class="key-name">' + escapeHTML(k.name || "未命名") + "</div>" +
            '<div class="key-secret">' + escapeHTML(k.masked || "") + "</div>" +
            '<div class="key-meta">' +
              status +
              '<span>Tokens ' + tokens + tlimit + "</span>" +
              '<span>Credits ' + credits + climit + "</span>" +
              '<span>最近使用 ' + last + "</span>" +
            "</div>" +
          "</div>" +
          '<div class="key-actions">' +
            '<button type="button" class="btn-mini" data-action="toggle">' +
              (k.enabled ? "禁用" : "启用") + "</button>" +
            '<button type="button" class="btn-mini danger" data-action="delete">删除</button>' +
          "</div>" +
        "</div>"
      );
    }).join("");
    box.querySelectorAll(".key-item").forEach(function (row) {
      var id = row.getAttribute("data-id");
      row.querySelectorAll("[data-action]").forEach(function (btn) {
        btn.addEventListener("click", function () {
          var action = btn.getAttribute("data-action");
          if (action === "toggle") {
            var nextEnabled = btn.textContent.trim() === "启用";
            api("/api/me/keys/" + encodeURIComponent(id), {
              method: "PUT",
              body: JSON.stringify({ enabled: nextEnabled }),
            }).then(loadMyKeys).catch(function (e) {
              showToast("操作失败：" + (e.message || ""));
            });
          } else if (action === "delete") {
            if (!confirm("确定要删除这个 Key 吗？删除后无法恢复。")) return;
            api("/api/me/keys/" + encodeURIComponent(id), { method: "DELETE" })
              .then(loadMyKeys)
              .catch(function (e) {
                showToast("删除失败：" + (e.message || ""));
              });
          }
        });
      });
    });
  }

  function bindKeyControls() {
    var newBtn = $("newKeyBtn");
    if (newBtn) {
      newBtn.addEventListener("click", function () {
        var name = prompt("为新 Key 起个名字（可留空）：", "");
        if (name === null) return;
        newBtn.disabled = true;
        api("/api/me/keys", {
          method: "POST",
          body: JSON.stringify({ name: (name || "").trim() }),
        }).then(function (d) {
          var k = d && d.key;
          if (k && k.key) showNewKey(k.key);
          loadMyKeys();
        }).catch(function (e) {
          showToast("创建失败：" + (e.message || ""));
        }).then(function () {
          newBtn.disabled = false;
        });
      });
    }
    var copy = $("copyNewKey");
    if (copy) {
      copy.addEventListener("click", function () {
        var v = $("newKeyValue");
        if (!v) return;
        safeCopy(v.textContent || "").then(function () {
          showToast("已复制完整 Key");
        });
      });
    }
    var dismiss = $("dismissNewKey");
    if (dismiss) {
      dismiss.addEventListener("click", function () {
        var box = $("newKeyBox");
        if (box) box.setAttribute("hidden", "");
      });
    }
  }

  function showNewKey(value) {
    var box = $("newKeyBox");
    var v = $("newKeyValue");
    if (!box || !v) return;
    v.textContent = value;
    box.removeAttribute("hidden");
  }

  /* ---------------------- Init ---------------------- */

  function init() {
    setEndpoints();
    bindCopy();
    loadStatus();
    loadModels();
    bindAuthModal();
    bindKeyControls();
    loadMe();
    setInterval(loadStatus, 30000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
