"use strict";

const profileURL = "/api/saas/me/profile-share";
const consentsURL = profileURL + "/consents";
const fields = [
  { key: "real_name", label: "Real name" },
  { key: "organization", label: "Organization" },
  { key: "country", label: "Country" },
];
const textVersion = document.body.dataset.textVersion;
const profileList = document.getElementById("profile-fields");
const hiveList = document.getElementById("consent-hives");
const profileStatus = document.getElementById("profile-status");
const consentsStatus = document.getElementById("consents-status");
let versionMatches = false;

function showStatus(node, message, error = false) {
  node.toggleAttribute("data-error", error);
  node.textContent = message;
}

async function request(url, method = "GET", body) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(url, {
    method, headers, credentials: "same-origin", cache: "no-store",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) {
    const data = await response.json().catch(() => null);
    throw new Error(data?.error || `Request failed (HTTP ${response.status})`);
  }
  return response.status === 204 ? null : response.json();
}

function renderProfileField(field, saved) {
  const form = document.getElementById("profile-field-template").content.firstElementChild.cloneNode(true);
  const input = form.querySelector("input");
  const clear = form.querySelector(".clear");
  const controls = form.querySelector("fieldset");
  const status = form.querySelector(".status");
  form.dataset.field = field.key;
  form.querySelector("h3").textContent = field.label;
  form.querySelector("label span").textContent = "Enter your " + field.label.toLowerCase();
  if (field.key === "country") {
    input.placeholder = "Two-letter code, e.g. US";
    input.setAttribute("aria-label", "Enter your two-letter country code");
  }

  function showValue({ value, source }) {
    form.querySelector(".current").textContent = value || "Not set";
    let provenance = "";
    if (value) {
      switch (source) {
        case "user": provenance = "You entered this."; break;
        case "admin": provenance = "An administrator entered this."; break;
        case "inferred": provenance = "Guessed from your browser."; break;
        default: provenance = "Source not recorded.";
      }
      if (source !== "user") provenance += " Not eligible for sharing until you enter it yourself.";
    }
    form.querySelector(".source").textContent = provenance;
    clear.disabled = !value;
  }

  async function save(value) {
    const focused = document.activeElement;
    controls.disabled = true;
    showStatus(status, "Saving…");
    try {
      const profile = await request(profileURL, "PUT", { [field.key]: value });
      showValue(profile[field.key]);
      input.value = "";
      showStatus(status, value ? `${field.label} saved.` : `${field.label} cleared.`);
    } catch (error) {
      showStatus(status, `Could not save: ${error.message}`, true);
    } finally {
      controls.disabled = false;
      if (document.activeElement === document.body) (focused === clear && clear.disabled ? input : focused).focus();
    }
  }

  form.addEventListener("submit", event => {
    event.preventDefault();
    const value = input.value.trim();
    if (value) save(value);
    else showStatus(status, "Enter a value to save or use Clear saved value.", true);
  });
  clear.addEventListener("click", () => save(""));
  showValue(saved);
  return form;
}

async function loadProfile() {
  try {
    const profile = await request(profileURL);
    profileList.replaceChildren(...fields.map(field => renderProfileField(field, profile[field.key])));
    showStatus(profileStatus, "");
  } catch (error) {
    showStatus(profileStatus, `Could not load your details: ${error.message}. Reload to try again.`, true);
  }
}

function setConsentBusy(busy) {
  hiveList.querySelectorAll("fieldset, button[type=submit]").forEach(node => {
    node.disabled = busy || !versionMatches;
  });
  hiveList.querySelectorAll(".withdraw").forEach(node => { node.disabled = busy; });
}

function scrollToHive(id) {
  const form = Array.from(hiveList.children).find(node => node.dataset.hiveId === id);
  if (!form) return false;
  form.querySelector("h3").focus();
  return true;
}

function renderConsentHive(hive) {
  const form = document.getElementById("consent-hive-template").content.firstElementChild.cloneNode(true);
  const name = hive.name || hive.hive_id;
  const consent = hive.consent;
  const status = form.querySelector(".status");
  const choices = form.querySelector(".choices");
  const withdraw = form.querySelector(".withdraw");
  const recorded = consent && !consent.withdrawn_at;
  form.dataset.hiveId = hive.hive_id;
  form.querySelector("h3").textContent = name;
  form.setAttribute("aria-label", `Profile sharing for ${name}`);
  withdraw.hidden = !recorded;
  let selection = "No consent recorded.";
  if (consent?.withdrawn_at) selection = "Consent withdrawn.";
  else if (recorded) {
    const labels = fields.filter(field => consent.fields[field.key]).map(field => field.label).join(", ");
    const suspended = !versionMatches || consent.text_version !== textVersion;
    selection = suspended
      ? `Suspended — reconfirm under the current disclosure. Previous selection: ${labels}.`
      : `Recorded selection: ${labels}.`;
  }
  form.querySelector(".saved-selection").textContent = selection;

  for (const field of fields) {
    const label = document.createElement("label");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.name = field.key;
    label.append(input, document.createTextNode(field.label));
    choices.append(label);
  }

  async function updateConsent(method, body) {
    const focused = document.activeElement;
    setConsentBusy(true);
    showStatus(status, method === "POST" ? "Saving…" : "Withdrawing…");
    try {
      await request(consentsURL + "/" + encodeURIComponent(hive.hive_id), method, body);
    } catch (error) {
      showStatus(status, `Could not ${method === "POST" ? "save consent" : "withdraw consent"}: ${error.message}`, true);
      setConsentBusy(false);
      if (document.activeElement === document.body) focused.focus();
      return;
    }
    const done = method === "POST" ? `Selection saved for ${name}.` : `Consent withdrawn for ${name}.`;
    form.reset();
    showStatus(status, "");
    try {
      renderConsentList(await request(consentsURL), done);
      if (document.activeElement === document.body) scrollToHive(hive.hive_id);
    } catch (error) {
      showStatus(consentsStatus, `${done} Could not refresh the hive list: ${error.message}. Reload before making more changes.`, true);
    }
  }

  form.addEventListener("submit", event => {
    event.preventDefault();
    if (!versionMatches) return;
    const selected = Object.fromEntries(Array.from(choices.querySelectorAll("input"), input => [input.name, input.checked]));
    if (!Object.values(selected).some(Boolean)) {
      showStatus(status, "Select at least one field to confirm. To remove a previous selection, use Withdraw consent.", true);
      return;
    }
    updateConsent("POST", { text_version: textVersion, fields: selected });
  });
  withdraw.addEventListener("click", () => updateConsent("DELETE"));
  return form;
}

function renderConsentList(data, message = "") {
  versionMatches = data.text_version === textVersion;
  const hives = data.hives || [];
  hiveList.replaceChildren(...hives.map(renderConsentHive));
  setConsentBusy(false);
  if (!versionMatches) {
    showStatus(consentsStatus, `${message ? message + " " : ""}The disclosure has changed. Reload the page to review it before confirming a selection.`, true);
  } else {
    showStatus(consentsStatus, message || (hives.length ? "" : "You don’t have a current access grant for any hive."));
  }
}

async function loadConsents() {
  try {
    renderConsentList(await request(consentsURL));
    const target = new URLSearchParams(location.search).get("hive");
    if (target && !scrollToHive(target) && versionMatches) {
      showStatus(consentsStatus, "That hive isn’t in your current access grants.", true);
    }
  } catch (error) {
    showStatus(consentsStatus, `Could not load your hives: ${error.message}. Reload to try again.`, true);
  }
}

loadProfile();
loadConsents();
