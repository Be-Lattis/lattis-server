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

// data-show-if="champ=a,b" (ou "champ!=a") : l'élément ne s'affiche que si
// le champ du même formulaire vaut l'une des valeurs. Une case cochée vaut
// sa valeur (« on »), décochée « ». Sur une <option>, la masque aussi.
function fieldValue(scope, name) {
  const els = scope.querySelectorAll(`[name="${name}"]`);
  for (const el of els) {
    if (el.type === "checkbox" || el.type === "radio") { if (el.checked) return el.value; continue; }
    return el.value;
  }
  return "";
}
function applyShowIf() {
  for (const el of document.querySelectorAll("[data-show-if]")) {
    const m = el.dataset.showIf.match(/^([\w-]+)(!?=)(.*)$/);
    if (!m) continue;
    const scope = el.closest("form") || document;
    const hit = m[3].split(",").includes(fieldValue(scope, m[1]));
    const show = m[2] === "=" ? hit : !hit;
    el.hidden = !show;
    if (el.tagName === "OPTION") {
      const sel = el.parentElement;
      if (!show && el.selected) {
        const first = [...sel.options].find((o) => !o.hidden && o !== el);
        if (first) first.selected = true;
      }
    } else {
    }
  }
}
document.addEventListener("change", applyShowIf);
document.addEventListener("input", applyShowIf);
applyShowIf();
