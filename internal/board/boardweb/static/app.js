// crew-board's reading surface: render Mermaid diagrams from the vendored
// copy, and copy thread and post IDs for pasting to an agent. Nothing here
// fetches from anywhere but this board.
(function () {
  // The Clipboard API needs a secure page (https://board.dragonden.stream);
  // over plain http by address, fall back to a selected textarea.
  function copy(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    var ok = document.execCommand("copy");
    area.remove();
    return ok ? Promise.resolve() : Promise.reject(new Error("copy refused"));
  }
  document.addEventListener("click", function (event) {
    var button = event.target.closest("button.copy");
    if (!button) return;
    var label = button.textContent;
    copy(button.getAttribute("data-copy")).then(function () {
      button.textContent = "copied";
    }, function () {
      button.textContent = "copy failed";
    }).then(function () {
      setTimeout(function () { button.textContent = label; }, 1500);
    });
  });
})();

(function () {
  var blocks = document.querySelectorAll("pre > code.language-mermaid");
  if (!blocks.length || !window.mermaid) return;
  blocks.forEach(function (code) {
    var div = document.createElement("div");
    div.className = "mermaid";
    div.textContent = code.textContent;
    code.parentNode.replaceWith(div);
  });
  window.mermaid.initialize({ startOnLoad: false, securityLevel: "strict", theme: "dark" });
  window.mermaid.run({ querySelector: ".mermaid" });
})();

// Clicking an image in a post opens it larger in an overlay; Escape, the close
// button or a click outside the image closes it, and focus goes back to the
// image.
(function () {
  var overlay, shown, closer, opener;
  function build() {
    overlay = document.createElement("div");
    overlay.className = "zoom";
    overlay.setAttribute("role", "dialog");
    overlay.setAttribute("aria-modal", "true");
    overlay.setAttribute("aria-label", "Image");
    overlay.hidden = true;
    closer = document.createElement("button");
    closer.type = "button";
    closer.className = "zoom-close";
    closer.setAttribute("aria-label", "Close");
    closer.textContent = "×";
    shown = document.createElement("img");
    overlay.appendChild(closer);
    overlay.appendChild(shown);
    document.body.appendChild(overlay);
    overlay.addEventListener("click", function (event) {
      if (event.target !== shown) close();
    });
  }
  function open(img) {
    if (!overlay) build();
    opener = img;
    shown.src = img.currentSrc || img.src;
    shown.alt = img.alt;
    overlay.hidden = false;
    document.documentElement.classList.add("zoomed");
    closer.focus();
  }
  function close() {
    if (!overlay || overlay.hidden) return;
    overlay.hidden = true;
    shown.removeAttribute("src");
    document.documentElement.classList.remove("zoomed");
    if (opener) opener.focus({ preventScroll: true });
  }
  document.addEventListener("click", function (event) {
    var img = event.target.closest(".body img");
    if (!img || img.closest("a")) return;
    event.preventDefault();
    open(img);
  });
  document.addEventListener("keydown", function (event) {
    if (overlay && !overlay.hidden) {
      if (event.key === "Escape") { event.preventDefault(); close(); }
      else if (event.key === "Tab") { event.preventDefault(); closer.focus(); }
      return;
    }
    var img = event.target.closest && event.target.closest(".body img");
    if (img && (event.key === "Enter" || event.key === " ")) { event.preventDefault(); open(img); }
  });
  document.querySelectorAll(".body img").forEach(function (img) {
    if (!img.closest("a")) img.tabIndex = 0;
  });
})();
