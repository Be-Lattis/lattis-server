// Formulaire Lattis : tout marche sans ce script ; il ajoute la position,
// le brouillon gardé sur l'appareil et « Réessayer » sans réseau.
"use strict";
(function () {
  const form = document.querySelector("form[data-draft]");
  if (!form) return;
  const key = "lattis-form:" + location.pathname;

  // Brouillon : la saisie survit à un rechargement ou à une coupure.
  const fields = () => [...form.elements].filter((e) => e.name && e.name !== "t" && e.name !== "website" && e.type !== "hidden" && !e.readOnly && !e.disabled);
  let draft = null;
  try { draft = JSON.parse(localStorage.getItem(key) || "null"); } catch (e) { /* stockage indisponible */ }
  const hasError = !!document.querySelector(".field.invalid, .note.error");
  if (draft && !hasError) {
    for (const el of fields()) {
      if (!(el.name in draft)) continue;
      if (el.type === "checkbox") el.checked = draft[el.name] === true;
      else if (el.type === "radio") el.checked = draft[el.name] === el.value;
      else if (el.value === "" || el.value === el.defaultValue) el.value = draft[el.name];
    }
  }
  const save = () => {
    const d = {};
    for (const el of fields()) {
      if (el.type === "checkbox") d[el.name] = el.checked;
      else if (el.type === "radio") { if (el.checked) d[el.name] = el.value; }
      else d[el.name] = el.value;
    }
    try { localStorage.setItem(key, JSON.stringify(d)); } catch (e) { /* stockage indisponible */ }
  };
  form.addEventListener("input", save);
  form.addEventListener("change", save);

  // Envoi : sans réseau, la réponse reste ici avec « Réessayer ».
  const offline = form.querySelector("[data-offline]");
  form.addEventListener("submit", (ev) => {
    if (navigator.onLine === false && offline) {
      ev.preventDefault();
      offline.hidden = false;
      return;
    }
    save();
  });
  form.querySelector("[data-retry]")?.addEventListener("click", () => {
    if (navigator.onLine !== false) form.requestSubmit();
  });
  // Page de remerciement atteinte : le brouillon n'a plus lieu d'être.
  window.addEventListener("pageshow", () => {
    if (document.querySelector(".done")) try { localStorage.removeItem(key); } catch (e) { /* rien */ }
  });

  // Position : remplie seulement sur demande du répondant.
  for (const box of document.querySelectorAll("[data-position]")) {
    const btn = box.querySelector("[data-locate]");
    const where = box.querySelector("[data-where]");
    const [wait, found, none] = (where.dataset.texts || "").split("|");
    btn.addEventListener("click", () => {
      if (!navigator.geolocation) { where.textContent = none; return; }
      where.textContent = wait;
      navigator.geolocation.getCurrentPosition((p) => {
        box.querySelector("[data-lat]").value = p.coords.latitude.toFixed(6);
        box.querySelector("[data-lon]").value = p.coords.longitude.toFixed(6);
        where.textContent = found.replace("%s", Math.round(p.coords.accuracy));
        save();
      }, () => { where.textContent = none; }, { enableHighAccuracy: true, timeout: 20000, maximumAge: 60000 });
    });
  }
})();
// Remerciement : efface le brouillon du formulaire.
if (document.querySelector(".done")) {
  try { localStorage.removeItem("lattis-form:" + location.pathname.replace(/\/merci$/, "")); } catch (e) { /* rien */ }
}
