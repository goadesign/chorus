// Copy the visible installation text and report either success or a manual step.
for (const button of document.querySelectorAll("button[data-copy]")) {
  button.hidden = false;
  button.addEventListener("click", async () => {
    const command = document.getElementById(button.dataset.copy);
    const result = document.getElementById("copy-status");
    try {
      await navigator.clipboard.writeText(command.textContent);
      result.textContent = "Copied. Paste the command into your terminal.";
    } catch {
      const range = document.createRange();
      range.selectNodeContents(command);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      result.textContent = "Copy was blocked. The command is selected; use your browser’s Copy command.";
    }
  });
}
