// Small helpers for the pages. The Content-Security-Policy blocks inline
// scripts, so page behaviour lives here and is wired with data- attributes.

// A button with data-copy="<element id>" copies that input's value.
document.addEventListener("click", async (event) => {
	const button = event.target.closest("[data-copy]");
	if (!button) {
		return;
	}
	const input = document.getElementById(button.dataset.copy);
	if (!input) {
		return;
	}
	try {
		await navigator.clipboard.writeText(input.value);
	} catch {
		// Clipboard access needs HTTPS or localhost. Select the text so
		// the person can copy it by hand.
		input.select();
		return;
	}
	const label = button.textContent;
	button.textContent = "Copied";
	setTimeout(() => {
		button.textContent = label;
	}, 1500);
});
