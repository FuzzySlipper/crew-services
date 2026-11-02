// crew-board's reading surface: render Mermaid diagrams from the vendored
// copy. Nothing here fetches from anywhere but this board.
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
