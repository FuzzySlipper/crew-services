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
