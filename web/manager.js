(function () {
  "use strict";

  var SESSION_NAME_RE = /^[a-zA-Z0-9_.-]+$/;
  var RESERVED_NAMES = {
    "list": true,
    "manager": true,
    "api": true,
    "ws": true,
    "login": true,
    "logout": true,
    "auth": true,
    "js": true,
    "css": true,
    "static": true,
    "favicon.png": true,
    "favicon.ico": true,
    "apple-touch-icon.png": true,
    "auth_token.js": true,
    "config.js": true,
    "robots.txt": true
  };
  var REFRESH_INTERVAL_MS = 5000;

  function getBasePath() {
    var path = window.location.pathname || "";
    return path.replace(/\/manager\/?$/, "");
  }

  var basePath = getBasePath();

  function apiUrl(subPath) {
    return basePath + subPath;
  }

  function sessionUrl(name) {
    return basePath + "/" + encodeURIComponent(name);
  }

  // DOM elements
  var serverBadge = document.getElementById("server-badge");
  var serverBadgeText = document.getElementById("server-badge-text");
  var autoRefreshCheckbox = document.getElementById("auto-refresh-checkbox");
  var refreshBtn = document.getElementById("refresh-btn");
  var refreshIcon = document.getElementById("refresh-icon");
  var logoutBtn = document.getElementById("logout-btn");

  var statTotal = document.getElementById("stat-total");
  var statAttached = document.getElementById("stat-attached");
  var statDetached = document.getElementById("stat-detached");

  var errorBanner = document.getElementById("error-banner");
  var errorBannerMessage = document.getElementById("error-banner-message");
  var errorRetryBtn = document.getElementById("error-retry-btn");

  var lastUpdated = document.getElementById("last-updated");
  var sessionsTable = document.getElementById("sessions-table");
  var sessionsTbody = document.getElementById("sessions-tbody");
  var emptyState = document.getElementById("empty-state");

  var openCreateBtn = document.getElementById("open-create-btn");
  var emptyCreateBtn = document.getElementById("empty-create-btn");
  var footerCreateBtn = document.getElementById("footer-create-btn");

  var createModal = document.getElementById("create-modal");
  var createModalClose = document.getElementById("create-modal-close");
  var createSessionForm = document.getElementById("create-session-form");
  var sessionNameInput = document.getElementById("session-name-input");
  var createModalError = document.getElementById("create-modal-error");
  var createCancelBtn = document.getElementById("create-cancel-btn");
  var createSubmitBtn = document.getElementById("create-submit-btn");
  var createAndOpenBtn = document.getElementById("create-and-open-btn");

  var killModal = document.getElementById("kill-modal");
  var killModalClose = document.getElementById("kill-modal-close");
  var killConfirmQuestion = document.getElementById("kill-confirm-question");
  var killSessionCode = document.getElementById("kill-session-code");
  var killModalError = document.getElementById("kill-modal-error");
  var killCancelBtn = document.getElementById("kill-cancel-btn");
  var killConfirmBtn = document.getElementById("kill-confirm-btn");

  var toastContainer = document.getElementById("toast-container");

  var refreshTimer = null;
  var pendingKillSession = null;
  var isSubmitting = false;

  function showToast(message, type) {
    var toast = document.createElement("div");
    toast.className = "toast " + (type === "error" ? "toast-error" : "toast-success");
    toast.textContent = message;
    toastContainer.appendChild(toast);

    setTimeout(function () {
      if (toast.parentNode) {
        toast.parentNode.removeChild(toast);
      }
    }, 3600);
  }

  function validateSessionName(raw) {
    var name = (raw || "").trim();
    if (!name) {
      return "Please enter a session name.";
    }
    if (name.length > 128) {
      return "Session name must be 128 characters or fewer.";
    }
    if (name === "." || name === "..") {
      return "Session name cannot be '.' or '..'.";
    }
    if (!SESSION_NAME_RE.test(name)) {
      return "Invalid session name. Only letters, numbers, '_', '.', and '-' are allowed.";
    }
    if (RESERVED_NAMES[name.toLowerCase()]) {
      return "'" + name + "' is a reserved system route name.";
    }
    return null;
  }

  function updateServerBadge(status) {
    serverBadge.className = "server-badge";
    if (!status || !status.installed) {
      serverBadge.classList.add("server-missing");
      serverBadgeText.textContent = "tmux not installed";
      return;
    }
    if (status.server_running) {
      serverBadge.classList.add("server-running");
      serverBadgeText.textContent = "tmux running";
    } else {
      serverBadge.classList.add("server-idle");
      serverBadgeText.textContent = "tmux idle (no server)";
    }
  }

  function renderSessions(sessions) {
    var list = Array.isArray(sessions) ? sessions : [];
    var attachedCount = 0;
    for (var i = 0; i < list.length; i++) {
      if (list[i].attached > 0) {
        attachedCount++;
      }
    }

    statTotal.textContent = String(list.length);
    statAttached.textContent = String(attachedCount);
    statDetached.textContent = String(list.length - attachedCount);

    while (sessionsTbody.firstChild) {
      sessionsTbody.removeChild(sessionsTbody.firstChild);
    }

    if (list.length === 0) {
      sessionsTable.classList.add("hidden");
      emptyState.classList.remove("hidden");
      return;
    }

    emptyState.classList.add("hidden");
    sessionsTable.classList.remove("hidden");

    for (var j = 0; j < list.length; j++) {
      var s = list[j];
      var tr = document.createElement("tr");

      // Name column
      var tdName = document.createElement("td");
      tdName.setAttribute("data-label", "Name");
      var nameLink = document.createElement("a");
      nameLink.className = "session-name-link";
      nameLink.href = sessionUrl(s.name);
      nameLink.textContent = s.name;
      nameLink.title = "Open session " + s.name;
      tdName.appendChild(nameLink);
      tr.appendChild(tdName);

      // Windows column
      var tdWin = document.createElement("td");
      tdWin.setAttribute("data-label", "Windows");
      var winPill = document.createElement("span");
      winPill.className = "window-pill";
      winPill.textContent = String(s.windows);
      tdWin.appendChild(winPill);
      tr.appendChild(tdWin);

      // Status column
      var tdStatus = document.createElement("td");
      tdStatus.setAttribute("data-label", "Status");
      var badge = document.createElement("span");
      var dot = document.createElement("span");
      dot.className = "dot";
      badge.appendChild(dot);

      var statusText = document.createElement("span");
      if (s.attached > 0) {
        badge.className = "status-badge status-attached";
        statusText.textContent = s.attached > 1 ? "Attached (" + s.attached + ")" : "Attached";
      } else {
        badge.className = "status-badge status-detached";
        statusText.textContent = "Detached";
      }
      badge.appendChild(statusText);
      tdStatus.appendChild(badge);
      tr.appendChild(tdStatus);

      // Actions column
      var tdActions = document.createElement("td");
      tdActions.className = "col-actions";
      tdActions.setAttribute("data-label", "Actions");

      var group = document.createElement("div");
      group.className = "action-group";

      var openBtn = document.createElement("a");
      openBtn.className = "btn btn-sm btn-open";
      openBtn.href = sessionUrl(s.name);
      openBtn.textContent = "Open";
      openBtn.title = "Open session in current tab";
      group.appendChild(openBtn);

      var newTabBtn = document.createElement("a");
      newTabBtn.className = "btn btn-sm btn-secondary";
      newTabBtn.href = sessionUrl(s.name);
      newTabBtn.target = "_blank";
      newTabBtn.rel = "noopener noreferrer";
      newTabBtn.textContent = "New Tab \u2197";
      newTabBtn.title = "Open session in new browser tab";
      group.appendChild(newTabBtn);

      var killBtn = document.createElement("button");
      killBtn.type = "button";
      killBtn.className = "btn btn-sm btn-danger";
      killBtn.textContent = "Kill";
      killBtn.setAttribute("data-session-name", s.name);
      killBtn.addEventListener("click", onKillButtonClick);
      group.appendChild(killBtn);

      tdActions.appendChild(group);
      tr.appendChild(tdActions);

      sessionsTbody.appendChild(tr);
    }
  }

  function fetchDashboardData(showManualToast) {
    refreshIcon.classList.add("spin");
    refreshBtn.disabled = true;

    var listPromise = fetch(apiUrl("/list"), {
      method: "GET",
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    }).then(function (res) {
      if (res.status === 401) {
        window.location.href = apiUrl("/login?next=" + encodeURIComponent(window.location.pathname + window.location.search));
        return { sessions: [] };
      }
      return res.json().catch(function () { return {}; }).then(function (body) {
        if (!res.ok) {
          throw new Error(body.error || ("Failed to load sessions (HTTP " + res.status + ")"));
        }
        return body;
      });
    });

    var statusPromise = fetch(apiUrl("/api/tmux/status"), {
      method: "GET",
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    }).then(function (res) {
      if (!res.ok) {
        return null;
      }
      return res.json().catch(function () { return null; });
    }).catch(function () {
      return null;
    });

    return Promise.all([listPromise, statusPromise])
      .then(function (results) {
        var listData = results[0];
        var statusData = results[1];

        errorBanner.classList.add("hidden");
        renderSessions(listData.sessions || []);
        if (statusData) {
          updateServerBadge(statusData);
        } else {
          updateServerBadge({
            installed: true,
            server_running: (listData.sessions || []).length > 0,
            session_count: (listData.sessions || []).length
          });
        }

        var now = new Date();
        lastUpdated.textContent = "Updated " + now.toLocaleTimeString();
        if (showManualToast) {
          showToast("Session list refreshed", "success");
        }
      })
      .catch(function (err) {
        errorBannerMessage.textContent = err.message || "Unable to reach GoTTY server.";
        errorBanner.classList.remove("hidden");
        if (showManualToast) {
          showToast(err.message || "Refresh failed", "error");
        }
      })
      .finally(function () {
        refreshIcon.classList.remove("spin");
        refreshBtn.disabled = false;
      });
  }

  // Create Modal Logic
  function openCreateModal() {
    sessionNameInput.value = "";
    createModalError.textContent = "";
    createModalError.classList.add("hidden");
    createModal.classList.remove("hidden");
    setTimeout(function () {
      sessionNameInput.focus();
    }, 20);
  }

  function closeCreateModal() {
    if (isSubmitting) {
      return;
    }
    createModal.classList.add("hidden");
  }

  function submitCreateSession(openAfterCreate) {
    if (isSubmitting) {
      return;
    }

    var rawName = sessionNameInput.value.trim();
    var validationError = validateSessionName(rawName);
    if (validationError) {
      createModalError.textContent = validationError;
      createModalError.classList.remove("hidden");
      sessionNameInput.focus();
      return;
    }

    createModalError.classList.add("hidden");
    isSubmitting = true;
    createSubmitBtn.disabled = true;
    createAndOpenBtn.disabled = true;

    fetch(apiUrl("/api/tmux/sessions"), {
      method: "POST",
      credentials: "same-origin",
      headers: {
        "Content-Type": "application/json",
        "Accept": "application/json"
      },
      body: JSON.stringify({ name: rawName })
    })
      .then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (body) {
          return { status: res.status, ok: res.ok, body: body };
        });
      })
      .then(function (result) {
        isSubmitting = false;
        createSubmitBtn.disabled = false;
        createAndOpenBtn.disabled = false;

        if (!result.ok) {
          var errMsg = result.body.error || ("Failed to create session (HTTP " + result.status + ")");
          if (result.status === 409) {
            errMsg = "Session \"" + rawName + "\" already exists.";
          }
          createModalError.textContent = errMsg;
          createModalError.classList.remove("hidden");
          showToast(errMsg, "error");
          return;
        }

        var createdName = (result.body && result.body.name) ? result.body.name : rawName;
        createModal.classList.add("hidden");
        showToast("Created session \"" + createdName + "\"", "success");

        fetchDashboardData(false).then(function () {
          if (openAfterCreate) {
            window.location.href = sessionUrl(createdName);
          }
        });
      })
      .catch(function (err) {
        isSubmitting = false;
        createSubmitBtn.disabled = false;
        createAndOpenBtn.disabled = false;
        var msg = err.message || "Network error while creating session.";
        createModalError.textContent = msg;
        createModalError.classList.remove("hidden");
        showToast(msg, "error");
      });
  }

  // Kill Modal Logic
  function onKillButtonClick(ev) {
    var name = ev.currentTarget.getAttribute("data-session-name");
    if (!name) {
      return;
    }
    pendingKillSession = name;
    killConfirmQuestion.textContent = "Are you sure you want to kill session \"" + name + "\"?";
    killSessionCode.textContent = name;
    killModalError.textContent = "";
    killModalError.classList.add("hidden");
    killModal.classList.remove("hidden");
    killConfirmBtn.focus();
  }

  function closeKillModal() {
    if (isSubmitting) {
      return;
    }
    pendingKillSession = null;
    killModal.classList.add("hidden");
  }

  function confirmKillSession() {
    if (!pendingKillSession || isSubmitting) {
      return;
    }

    var targetName = pendingKillSession;
    isSubmitting = true;
    killConfirmBtn.disabled = true;
    killModalError.classList.add("hidden");

    fetch(apiUrl("/api/tmux/sessions/" + encodeURIComponent(targetName)), {
      method: "DELETE",
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    })
      .then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (body) {
          return { status: res.status, ok: res.ok, body: body };
        });
      })
      .then(function (result) {
        isSubmitting = false;
        killConfirmBtn.disabled = false;

        if (!result.ok) {
          var errMsg = result.body.error || ("Failed to kill session (HTTP " + result.status + ")");
          if (result.status === 404) {
            pendingKillSession = null;
            killModal.classList.add("hidden");
            showToast("Session \"" + targetName + "\" no longer exists.", "error");
            fetchDashboardData(false);
            return;
          }
          killModalError.textContent = errMsg;
          killModalError.classList.remove("hidden");
          showToast(errMsg, "error");
          return;
        }

        pendingKillSession = null;
        killModal.classList.add("hidden");
        showToast("Killed session \"" + targetName + "\"", "success");
        fetchDashboardData(false);
      })
      .catch(function (err) {
        isSubmitting = false;
        killConfirmBtn.disabled = false;
        var msg = err.message || "Network error while killing session.";
        killModalError.textContent = msg;
        killModalError.classList.remove("hidden");
        showToast(msg, "error");
      });
  }

  function syncAutoRefresh() {
    if (refreshTimer) {
      clearInterval(refreshTimer);
      refreshTimer = null;
    }
    if (autoRefreshCheckbox.checked) {
      refreshTimer = setInterval(function () {
        if (!document.hidden) {
          fetchDashboardData(false);
        }
      }, REFRESH_INTERVAL_MS);
    }
  }

  // Event bindings
  refreshBtn.addEventListener("click", function () {
    fetchDashboardData(true);
  });
  errorRetryBtn.addEventListener("click", function () {
    fetchDashboardData(true);
  });
  autoRefreshCheckbox.addEventListener("change", syncAutoRefresh);

  openCreateBtn.addEventListener("click", openCreateModal);
  emptyCreateBtn.addEventListener("click", openCreateModal);
  footerCreateBtn.addEventListener("click", openCreateModal);
  createModalClose.addEventListener("click", closeCreateModal);
  createCancelBtn.addEventListener("click", closeCreateModal);

  createSessionForm.addEventListener("submit", function (ev) {
    ev.preventDefault();
    submitCreateSession(false);
  });
  createAndOpenBtn.addEventListener("click", function () {
    submitCreateSession(true);
  });

  killModalClose.addEventListener("click", closeKillModal);
  killCancelBtn.addEventListener("click", closeKillModal);
  killConfirmBtn.addEventListener("click", confirmKillSession);

  createModal.addEventListener("click", function (ev) {
    if (ev.target === createModal) {
      closeCreateModal();
    }
  });
  killModal.addEventListener("click", function (ev) {
    if (ev.target === killModal) {
      closeKillModal();
    }
  });

  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") {
      if (!createModal.classList.contains("hidden")) {
        closeCreateModal();
      }
      if (!killModal.classList.contains("hidden")) {
        closeKillModal();
      }
    }
  });

  // Initial load
  if (logoutBtn) {
    fetch(apiUrl("/api/auth/status"), {
      method: "GET",
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (authInfo) {
        if (authInfo && authInfo.password_enabled) {
          logoutBtn.classList.remove("hidden");
        }
      })
      .catch(function () {});

    logoutBtn.addEventListener("click", function () {
      fetch(apiUrl("/logout"), {
        method: "POST",
        credentials: "same-origin",
        headers: { "Accept": "application/json" }
      }).finally(function () {
        window.location.href = apiUrl("/login");
      });
    });
  }

  fetchDashboardData(false);
  syncAutoRefresh();
})();
