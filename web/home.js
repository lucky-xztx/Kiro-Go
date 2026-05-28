/* Kiro-Go public homepage script. */
(function () {
  "use strict";

  var BASE = window.location.origin;

  function $(id) {
    return document.getElementById(id);
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
        // Dedup by id, prefer non-thinking variants first.
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
            var safe = String(id).replace(/[<>&"']/g, function (c) {
              return (
                { "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c] ||
                c
              );
            });
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

  function init() {
    setEndpoints();
    bindCopy();
    loadStatus();
    loadModels();
    setInterval(loadStatus, 30000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
