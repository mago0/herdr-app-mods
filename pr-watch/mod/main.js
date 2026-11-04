// The focused agent's GitHub PRs and its direct children's, from herdr-pr's registry.
// Opened as a popup with props { menu: { url, watched } }, it is a row's right-click menu.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { exec, home, focused, terminals, theme, props, openOverlay, closeOverlay, notify, watchFolder, shared, setShared } from "herdr";

const STATE = home() + "/.local/state/herdr-pr";
const EYE = "mods/pr-watch/eye.svg";
// Bumped by the menu after an action so every panel copy refreshes at once.
const REV = "pr-watch.rev";

// The focused pane's terminal first: `agent` only tracks agent layout blocks and can lag a space switch.
const agentOf = () => focused()?.terminal ?? focused()?.agent ?? null;

// Runs a command for an action; a failure (herdr-pr prints `[herdr-pr] ...` on stderr) becomes a toast.
async function act(command, args) {
  try {
    const r = await exec(command, args);
    if (r.code === 0) return true;
    notify({ title: "PR watch", body: (r.stderr || r.stdout).trim() || `${command} exited ${r.code}`, level: "error" });
  } catch (e) {
    notify({ title: "PR watch", body: String(e), level: "error" });
  }
  return false;
}

const words = (s) => s.charAt(0) + s.slice(1).toLowerCase().replace(/_/g, " ");

function menuItems(m) {
  return [
    ["Copy link", "mods/workspace-chrome/files.svg", () => act("wl-copy", [m.url])],
    ["Open", "mods/workspace-chrome/panel-right.svg", () => act("xdg-open", [m.url])],
    m.watched
      ? ["Stop watching", EYE, () => act("herdr-pr", ["unwatch", m.url])]
      : ["Watch for changes", EYE, () => act("herdr-pr", ["watch", m.url])],
    ["Unlink", "mods/workspace-chrome/x.svg", () => act("herdr-pr", ["unlink", m.url])],
  ];
}

function chip(t, e) {
  const state = e.draft && e.state === "OPEN" ? "draft" : (e.state ?? "").toLowerCase();
  const color = { draft: t.dim, open: t.done, merged: t.ansi5, closed: t.blocked }[state] ?? t.dim;
  if (!state) return null;
  return div().flex_none().px(6).h(17).rounded(5).bg(color + "26").text_size(10.5).font_semibold().text_color(color).flex().items_center().child(state);
}

function ciDot(t, ci) {
  const color = { SUCCESS: t.done, FAILURE: t.blocked, ERROR: t.blocked, PENDING: t.working, EXPECTED: t.working }[ci];
  return color ? div().size(7).flex_none().rounded(4).bg(color) : null;
}

export default class PrWatch extends View {
  init(_props, cx) {
    if (props()?.menu) {
      this.at = 0;
      this.handle = cx.focus_handle();
      this.handle.focus();
      return;
    }
    this.agent = undefined;
    this.data = null;
    this.rev = shared(REV);
    this.watch(cx);
  }

  // Wakes on registry writes. The folder appears with the first link, so a focus change retries a failed watch.
  watch(cx) {
    if (this.watching) return;
    this.watching = true;
    cx.spawn(async (cx) => {
      try {
        for (;;) {
          await watchFolder(STATE);
          this.refresh(cx);
        }
      } catch {
        this.watching = false;
      }
    });
  }

  refresh(cx) {
    const agent = this.agent;
    if (!agent) return;
    cx.spawn(async (cx) => {
      const fail = (e) => ({ code: -1, stdout: "", stderr: String(e) });
      const ls = await exec("herdr-pr", ["ls", "--agent", agent, "--with-children", "--json"]).catch(fail);
      // Focus moved on while this ran.
      if (agent !== this.agent) return;
      if (ls.code === 0) {
        try {
          this.data = JSON.parse(ls.stdout);
          this.error = null;
        } catch (e) {
          this.error = `herdr-pr ls: ${e}`;
        }
      } else this.error = (ls.stderr || ls.stdout).trim() || `herdr-pr ls exited ${ls.code}`;
      cx.notify();
    });
  }

  choose(i, cx) {
    const it = menuItems(props().menu)[i];
    if (!it) return;
    cx.spawn(async () => {
      await it[2]();
      setShared(REV, Date.now());
      closeOverlay(props().overlay);
    });
  }

  menu(t) {
    const list = menuItems(props().menu);
    return v_flex()
      .id("menu")
      .track_focus(this.handle)
      .w_full()
      .p(5)
      .bg(t.popover)
      .text_size(12.8)
      .on_key_down((e, cx) => {
        if (e.key === "down") this.at = (this.at + 1) % list.length;
        else if (e.key === "up") this.at = (this.at + list.length - 1) % list.length;
        else if (e.key === "enter") this.choose(this.at, cx);
        else if (e.key === "escape") closeOverlay(props().overlay);
        else return;
        cx.notify();
      })
      .children(list.map((it, i) =>
        v_flex()
          .when(i === list.length - 1, (el) => el.child(div().mx(7).my(4).h(1).bg(t.border)))
          .child(h_flex()
            .id(`mi-${i}`)
            .h(31)
            .px(8)
            .gap(9)
            .rounded(7)
            .items_center()
            .text_color(t.foreground)
            .when(i === this.at, (el) => el.bg(t.selected))
            .hover((s) => s.bg(t.hover))
            .on_click((_e, cx) => this.choose(i, cx))
            .child(svg(it[1]).size(14).flex_none().text_color(t.muted))
            .child(div().flex_1().child(it[0])))));
  }

  row(t, e, dim) {
    const dot = ciDot(t, e.ci);
    const state = chip(t, e);
    return v_flex()
      .id(`pr-${e.url}`)
      .px(8)
      .py(5)
      .gap(2)
      .flex_none()
      .rounded(8)
      .cursor_pointer()
      .when(dim, (el) => el.opacity(0.55))
      .hover((s) => s.bg(t.hover))
      .on_click((_e, cx) => cx.spawn(async () => act("xdg-open", [e.url])))
      .on_mouse_down("right", (_e, cx) => {
        cx.stop_propagation();
        openOverlay({ mod: "pr-watch", kind: "popup", width: 210, height: 150, props: { menu: { url: e.url, watched: e.watched } } });
      })
      .child(h_flex().gap(6).items_center()
        .child(div().flex_1().min_w(0).truncate().text_size(11.5).text_color(t.muted).child(`${e.repo}#${e.number}`))
        .when(e.watched, (el) => el.child(svg(EYE).size(13).flex_none().text_color(t.accent)))
        .when(Boolean(dot), (el) => el.child(dot))
        .when(Boolean(state), (el) => el.child(state)))
      .child(div().truncate().text_color(t.foreground).child(e.title || e.url))
      .when(Boolean(e.review), (el) => el.child(div().truncate().text_size(11).text_color(t.dim).child(words(e.review))));
  }

  header(t, label, dim = false) {
    return div().px(8).pt(10).pb(3).flex_none().truncate().text_size(11).font_medium().text_color(dim ? t.dim : t.muted).child(label);
  }

  render(cx) {
    const t = theme();
    if (props()?.menu) return this.menu(t);

    const agent = agentOf();
    if (agent !== this.agent) {
      this.agent = agent;
      this.data = null;
      this.error = null;
      this.watch(cx);
      this.refresh(cx);
    }
    const rev = shared(REV);
    if (rev !== this.rev) {
      this.rev = rev;
      this.refresh(cx);
    }

    const rows = [];
    let note = null;
    if (!agent) note = "Focus an agent to see its PRs";
    else if (this.error) note = this.error;
    else if (this.data) {
      const { own = [], children = [] } = this.data;
      const live = new Set(terminals().filter((x) => x.state === "live").map((x) => x.address));
      rows.push(this.header(t, "This agent"));
      if (own.length) rows.push(...own.map((e) => this.row(t, e, false)));
      else rows.push(div().px(8).py(3).text_size(12).text_color(t.dim).child("No linked PRs"));
      for (const g of children) {
        const dim = !live.has(g.owner.address);
        rows.push(this.header(t, `${g.owner.address || "background child"} · ${g.owner.kind}`, dim));
        rows.push(...g.prs.map((e) => this.row(t, e, dim)));
      }
    }

    return v_flex()
      .size_full()
      .min_h(0)
      .rounded(10)
      .overflow_hidden()
      .bg(t.background)
      .border_1()
      .border_color(t.border)
      .text_size(13)
      .child(h_flex()
        .px(16)
        .h(41)
        .pt(14)
        .pb(5)
        .flex_none()
        .items_center()
        .justify_between()
        .child(div().text_size(11.5).font_medium().text_color(t.dim).child("Pull requests"))
        .when(Boolean(agent), (el) => el.child(div().text_size(11).text_color(t.dim).child(agent))))
      .when(Boolean(note), (el) => el.child(div().px(16).py(4).text_size(12).text_color(t.dim).child(note)))
      .child(v_flex().id("pr-list").flex_1().min_h(0).px(8).pb(8).overflow_y_scrollbar().children(rows));
  }
}
