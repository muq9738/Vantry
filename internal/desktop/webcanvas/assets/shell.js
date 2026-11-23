(function () {
  "use strict";

  const params = new URLSearchParams(window.location.search);
  const canvasId = params.get("id") || "";
  const token = params.get("token") || "";
  const initialTheme = params.get("theme") === "dark" ? "dark" : "light";

  let seq = 0;
  let currentMode = "";
  let scale = 1;
  let panX = 0;
  let panY = 0;
  let isDark = initialTheme === "dark";
  let lastPayload = null;
  let selectedEl = null;

  const viewport = document.getElementById("viewport");
  const stage = document.getElementById("stage");
  const errorBanner = document.getElementById("error-banner");

  function applyTheme(dark) {
    isDark = !!dark;
    document.documentElement.setAttribute("data-theme", isDark ? "dark" : "light");
    if (window.mermaid) {
      try {
        window.mermaid.initialize({
          startOnLoad: false,
          securityLevel: "strict",
          suppressErrorRendering: true,
          theme: isDark ? "dark" : "default",
        });
      } catch (_) {}
    }
  }

  function report(evt) {
    if (!canvasId || !token) return;
    fetch("/bridge?id=" + encodeURIComponent(canvasId) + "&token=" + encodeURIComponent(token), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(evt),
    }).catch(() => {});
  }

  function showError(msg) {
    if (!msg) {
      errorBanner.style.display = "none";
      errorBanner.textContent = "";
      return;
    }
    errorBanner.textContent = String(msg);
    errorBanner.style.display = "block";
    report({ type: "error", message: String(msg) });
  }

  function applyTransform() {
    if (currentMode === "html" || currentMode === "markdown") {
      stage.style.transform = "";
      return;
    }
    stage.style.transform = "translate(" + panX.toFixed(1) + "px, " + panY.toFixed(1) + "px) scale(" + scale.toFixed(3) + ")";
  }

  function fitStage() {
    scale = 1;
    panX = 0;
    panY = 0;
    const svg = stage.querySelector("svg");
    if (svg && viewport.clientWidth > 40 && viewport.clientHeight > 40) {
      const rect = svg.getBoundingClientRect();
      if (rect.width > 0 && rect.height > 0) {
        const sx = (viewport.clientWidth - 40) / rect.width;
        const sy = (viewport.clientHeight - 40) / rect.height;
        scale = Math.max(0.25, Math.min(2.0, Math.min(sx, sy)));
      }
    }
    applyTransform();
  }

  function describeElement(el) {
    if (!el || el === stage) return null;
    const txt = (el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 120);
    let sel = el.tagName ? el.tagName.toLowerCase() : "element";
    if (el.id) {
      sel += "#" + el.id;
    } else if (typeof el.className === "string" && el.className.trim()) {
      const cls = el.className.trim().split(/\s+/).slice(0, 2).join(".");
      if (cls) sel += "." + cls;
    } else if (el.className && typeof el.className.baseVal === "string" && el.className.baseVal.trim()) {
      const cls = el.className.baseVal.trim().split(/\s+/).slice(0, 2).join(".");
      if (cls) sel += "." + cls;
    }
    return { selector: sel, text: txt };
  }

  stage.addEventListener("click", function (ev) {
    if (currentMode !== "mermaid" && currentMode !== "svg") return;
    const node = ev.target.closest("g.node, g.cluster, g.actor, g.edgeLabel, g[id], text, rect, path, circle, polygon") || ev.target;
    const info = describeElement(node);
    if (!info) return;
    if (selectedEl && selectedEl !== node) {
      selectedEl.classList.remove("vantry-selected");
    }
    selectedEl = node;
    selectedEl.classList.add("vantry-selected");
    report({ type: "select", selector: info.selector, text: info.text });
  });

  window.addEventListener("message", function (ev) {
    const data = ev.data;
    if (!data || typeof data !== "object") return;
    if (data.type === "vantry-select") {
      report({
        type: "select",
        selector: String(data.selector || ""),
        text: String(data.text || "").slice(0, 160),
      });
    } else if (data.type === "vantry-height") {
      const h = Number(data.height);
      if (Number.isFinite(h) && h > 0) {
        report({ type: "resize", height: h });
      }
    }
  });

  let dragStart = null;
  viewport.addEventListener("pointerdown", function (ev) {
    if (currentMode === "html" || currentMode === "markdown" || ev.button !== 0) return;
    dragStart = { x: ev.clientX - panX, y: ev.clientY - panY, moved: false };
    viewport.classList.add("dragging");
  });
  window.addEventListener("pointermove", function (ev) {
    if (!dragStart) return;
    const nx = ev.clientX - dragStart.x;
    const ny = ev.clientY - dragStart.y;
    if (Math.hypot(nx - panX, ny - panY) > 3) dragStart.moved = true;
    panX = nx;
    panY = ny;
    applyTransform();
  });
  window.addEventListener("pointerup", function () {
    dragStart = null;
    viewport.classList.remove("dragging");
  });
  viewport.addEventListener("wheel", function (ev) {
    if (currentMode === "html" || currentMode === "markdown") return;
    if (ev.ctrlKey || ev.metaKey || currentMode === "mermaid" || currentMode === "svg") {
      ev.preventDefault();
      const factor = ev.deltaY < 0 ? 1.08 : 1 / 1.08;
      scale = Math.max(0.2, Math.min(5.0, scale * factor));
      applyTransform();
    }
  }, { passive: false });

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;" }[c];
    });
  }

  function renderBasicMarkdownWithMath(src) {
    const tokens = [];
    let text = String(src || "").replace(/\$\$([\s\S]+?)\$\$/g, function (_, tex) {
      const idx = tokens.length;
      try {
        tokens.push('<div class="math-display">' + window.katex.renderToString(tex, { displayMode: true, throwOnError: false }) + "</div>");
      } catch (_) {
        tokens.push("<pre>" + escapeHTML(tex) + "</pre>");
      }
      return "\u0000M" + idx + "\u0000";
    });
    text = text.replace(/\$([^\n$]+?)\$/g, function (_, tex) {
      const idx = tokens.length;
      try {
        tokens.push(window.katex.renderToString(tex, { displayMode: false, throwOnError: false }));
      } catch (_) {
        tokens.push("<code>" + escapeHTML(tex) + "</code>");
      }
      return "\u0000M" + idx + "\u0000";
    });
    const lines = text.split("\n");
    const out = [];
    for (let i = 0; i < lines.length; i++) {
      let ln = escapeHTML(lines[i]);
      if (ln.startsWith("### ")) out.push("<h3>" + ln.slice(4) + "</h3>");
      else if (ln.startsWith("## ")) out.push("<h2>" + ln.slice(3) + "</h2>");
      else if (ln.startsWith("# ")) out.push("<h1>" + ln.slice(2) + "</h1>");
      else if (ln.trim() === "") out.push("<div style='height:8px'></div>");
      else out.push("<p>" + ln + "</p>");
    }
    return '<div class="md-doc">' + out.join("\n").replace(/\u0000M(\d+)\u0000/g, function (_, idx) { return tokens[Number(idx)] || ""; }) + "</div>";
  }

  window.__vantrySetTheme = function (dark) {
    applyTheme(dark);
    if (lastPayload && (lastPayload.kind === "mermaid" || lastPayload.kind === "html")) {
      window.__vantryRender(Object.assign({}, lastPayload, { dark: !!dark }));
    }
  };

  window.__vantryZoom = function (cmd) {
    if (cmd === "in") scale = Math.min(5.0, scale * 1.2);
    else if (cmd === "out") scale = Math.max(0.2, scale / 1.2);
    else if (cmd === "reset") { scale = 1; panX = 0; panY = 0; }
    else if (cmd === "fit") { fitStage(); return scale; }
    else if (typeof cmd === "number" && Number.isFinite(cmd)) scale = Math.max(0.2, Math.min(5.0, cmd));
    applyTransform();
    return scale;
  };

  window.__vantryExportSVG = function () {
    const svg = stage.querySelector("svg");
    return svg ? svg.outerHTML : "";
  };

  window.__vantryRender = async function (payload) {
    lastPayload = payload;
    showError("");
    applyTheme(payload && typeof payload.dark === "boolean" ? payload.dark : isDark);
    const kind = String((payload && payload.kind) || "").toLowerCase();
    const source = String((payload && payload.source) || "");
    currentMode = kind;
    selectedEl = null;
    scale = 1;
    panX = 0;
    panY = 0;
    viewport.className = "mode-" + kind;
    stage.innerHTML = "";
    applyTransform();

    try {
      if (kind === "mermaid") {
        const uid = "v-mmd-" + (++seq);
        const res = await window.mermaid.render(uid, source);
        stage.innerHTML = res.svg;
        requestAnimationFrame(() => {
          const rect = stage.getBoundingClientRect();
          report({ type: "ready", width: rect.width, height: rect.height });
        });
      } else if (kind === "svg") {
        stage.innerHTML = source;
        requestAnimationFrame(() => {
          const svg = stage.querySelector("svg");
          const rect = (svg || stage).getBoundingClientRect();
          report({ type: "ready", width: rect.width, height: rect.height });
        });
      } else if (kind === "html") {
        const iframe = document.createElement("iframe");
        iframe.className = "sandbox-frame";
        iframe.setAttribute("sandbox", "allow-scripts");
        iframe.setAttribute("referrerpolicy", "no-referrer");
        iframe.srcdoc = source;
        stage.appendChild(iframe);
        report({ type: "ready", width: viewport.clientWidth, height: viewport.clientHeight });
      } else if (kind === "math") {
        const wrap = document.createElement("div");
        wrap.className = "math-display";
        window.katex.render(source, wrap, { displayMode: true, throwOnError: false });
        stage.appendChild(wrap);
        const rect = wrap.getBoundingClientRect();
        report({ type: "ready", width: rect.width, height: rect.height });
      } else if (kind === "markdown") {
        stage.innerHTML = renderBasicMarkdownWithMath(source);
        report({ type: "ready", width: stage.scrollWidth, height: stage.scrollHeight });
      } else {
        showError("不支持的画布类型: " + kind);
      }
    } catch (err) {
      showError(err && err.message ? err.message : String(err));
    }
    return true;
  };

  applyTheme(isDark);
  report({ type: "loaded" });
})();
