// Champs existants : bloqués jusqu'au clic sur leur crayon. Sans script,
// ils restent simplement modifiables.
document.documentElement.classList.add("js");
for (const c of document.querySelectorAll(".control")) {
  const inputs = c.querySelectorAll("input, select, textarea");
  const pen = c.querySelector(".pen");
  if (!pen) continue;
  for (const i of inputs) i.disabled = true;
  c.classList.add("locked");
  pen.addEventListener("click", () => {
    for (const i of inputs) i.disabled = false;
    c.classList.replace("locked", "editing");
    c.closest("form")?.classList.add("dirty");
    inputs[0]?.focus();
  });
}
// Un champ bloqué part quand même avec le formulaire (valeur inchangée).
document.addEventListener("submit", (e) => {
  for (const i of e.target.querySelectorAll(".control :disabled")) i.disabled = false;
}, true);
