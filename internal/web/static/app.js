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

// A button with data-confirm asks before its form is sent.
document.addEventListener("click", (event) => {
	const button = event.target.closest("[data-confirm]");
	if (button && !window.confirm(button.dataset.confirm)) {
		event.preventDefault();
	}
});

// Permission checkboxes: ticking one also ticks what it needs, and locks
// those until nothing ticked needs them. Each box lists its needs in
// data-requires, as sent by the server, so the rules live in one place.
// The server checks the set again on save.
function syncPermissions(list) {
	const boxes = [...list.querySelectorAll("input[data-requires]")];
	const byName = new Map(boxes.map((box) => [box.value, box]));
	const needed = new Set();
	const visit = (box) => {
		for (const name of box.dataset.requires.split(" ").filter(Boolean)) {
			if (!needed.has(name)) {
				needed.add(name);
				const next = byName.get(name);
				if (next) {
					visit(next);
				}
			}
		}
	};
	boxes.filter((box) => box.checked).forEach(visit);
	for (const box of boxes) {
		const locked = needed.has(box.value);
		if (locked) {
			box.checked = true;
		}
		box.dataset.locked = locked ? "true" : "false";
		box.setAttribute("aria-disabled", locked || box.disabled ? "true" : "false");
	}
}

for (const list of document.querySelectorAll("[data-permissions]")) {
	syncPermissions(list);
	list.addEventListener("change", () => syncPermissions(list));
	// A locked box stays ticked. It is not disabled, because a disabled
	// box would not be sent with the form.
	list.addEventListener("click", (event) => {
		if (event.target.matches('input[data-locked="true"]')) {
			event.preventDefault();
		}
	});
}
