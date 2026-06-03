"use strict";
(() => {
    const el = document.getElementById("route-canvas");
    if (!el)
        return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches)
        return;
    const ctxOrNull = el.getContext("2d");
    if (!ctxOrNull)
        return;
    const ctx = ctxOrNull;
    const DPR = Math.min(window.devicePixelRatio || 1, 2);
    let cw = 0;
    let ch = 0;
    const ro = new ResizeObserver((entries) => {
        const e = entries[0];
        if (!e)
            return;
        cw = e.contentRect.width;
        ch = e.contentRect.height;
        el.width = Math.round(cw * DPR);
        el.height = Math.round(ch * DPR);
        ctx.setTransform(DPR, 0, 0, DPR, 0, 0);
    });
    ro.observe(el);
    const clients = [
        { fx: 0.1, fy: 0.25, label: "app", kind: "client", glow: 0 },
        { fx: 0.1, fy: 0.5, label: "cli", kind: "client", glow: 0 },
        { fx: 0.1, fy: 0.75, label: "web", kind: "client", glow: 0 },
    ];
    const hub = { fx: 0.5, fy: 0.5, label: "llmesh", kind: "router", glow: 0 };
    const workers = [
        { fx: 0.88, fy: 0.25, label: "llama 3.2", kind: "worker", glow: 0 },
        { fx: 0.88, fy: 0.5, label: "qwen3", kind: "worker", glow: 0 },
        { fx: 0.88, fy: 0.75, label: "gemma 2", kind: "worker", glow: 0 },
    ];
    const allNodes = [...clients, hub, ...workers];
    const palette = [
        [100, 160, 255],
        [160, 200, 255],
        [100, 220, 180],
        [190, 170, 255],
    ];
    let packets = [];
    let nextSpawn = 0;
    let rafId = 0;
    let prevTs = 0;
    const nx = (n) => n.fx * cw;
    const ny = (n) => n.fy * ch;
    function ease(t) {
        return t < 0.5 ? 2 * t * t : -1 + (4 - 2 * t) * t;
    }
    function spawn(now) {
        if (packets.length >= 5)
            return;
        const src = clients[Math.floor(Math.random() * clients.length)];
        const dst = workers[Math.floor(Math.random() * workers.length)];
        const [r, g, b] = palette[Math.floor(Math.random() * palette.length)];
        packets.push({
            src, dst, phase: "go", t: 0,
            speed: 0.45 + Math.random() * 0.25,
            r, g, b, opacity: 1,
        });
        src.glow = Math.min(1, src.glow + 0.8);
        nextSpawn = now + 1300 + Math.random() * 1100;
    }
    function update(dt) {
        const kept = [];
        for (const p of packets) {
            if (p.phase === "go") {
                p.t += dt * p.speed;
                if (p.t >= 1) {
                    p.t = 0;
                    p.phase = "wait";
                    hub.glow = Math.min(1, hub.glow + 0.9);
                }
            }
            else if (p.phase === "wait") {
                p.t += dt * 3.5;
                if (p.t >= 1) {
                    p.t = 0;
                    p.phase = "fly";
                }
            }
            else if (p.phase === "fly") {
                p.t += dt * p.speed;
                if (p.t >= 1) {
                    p.t = 0;
                    p.phase = "die";
                    p.dst.glow = Math.min(1, p.dst.glow + 0.9);
                }
            }
            else {
                p.opacity -= dt * 2.5;
            }
            if (p.opacity > 0)
                kept.push(p);
        }
        packets = kept;
        for (const n of allNodes)
            n.glow = Math.max(0, n.glow - dt * 1.6);
    }
    function drawLines() {
        ctx.strokeStyle = "rgba(255,255,255,0.07)";
        ctx.lineWidth = 1;
        for (const c of clients) {
            ctx.beginPath();
            ctx.moveTo(nx(c), ny(c));
            ctx.lineTo(nx(hub), ny(hub));
            ctx.stroke();
        }
        for (const w of workers) {
            ctx.beginPath();
            ctx.moveTo(nx(hub), ny(hub));
            ctx.lineTo(nx(w), ny(w));
            ctx.stroke();
        }
    }
    function drawColumnLabels() {
        const fs = Math.max(8, Math.min(10, cw / 90));
        ctx.font = `${fs}px monospace`;
        ctx.fillStyle = "rgba(74,96,128,0.8)";
        ctx.textBaseline = "top";
        ctx.textAlign = "center";
        ctx.fillText("clients", clients[0].fx * cw, 6);
        ctx.fillText("workers", workers[0].fx * cw, 6);
    }
    function drawNode(n) {
        const x = nx(n);
        const y = ny(n);
        const isR = n.kind === "router";
        const isW = n.kind === "worker";
        const rad = isR ? 12 : 6;
        const [nr, ng, nb] = isR
            ? [124, 134, 200]
            : isW
                ? [80, 200, 140]
                : [100, 160, 255];
        if (n.glow > 0.02) {
            const glowR = rad + 16 * n.glow;
            const g = ctx.createRadialGradient(x, y, rad * 0.4, x, y, glowR);
            g.addColorStop(0, `rgba(${nr},${ng},${nb},${0.4 * n.glow})`);
            g.addColorStop(1, `rgba(${nr},${ng},${nb},0)`);
            ctx.beginPath();
            ctx.arc(x, y, glowR, 0, Math.PI * 2);
            ctx.fillStyle = g;
            ctx.fill();
        }
        ctx.beginPath();
        ctx.arc(x, y, rad, 0, Math.PI * 2);
        ctx.fillStyle = `rgba(${nr},${ng},${nb},${isR ? 0.9 : 0.75})`;
        ctx.fill();
        const fs = Math.max(9, Math.min(11, cw / 78));
        ctx.font = `${fs}px monospace`;
        ctx.textAlign = "center";
        ctx.textBaseline = "top";
        ctx.fillStyle = "rgba(200,211,232,0.8)";
        ctx.fillText(n.label, x, y + rad + 4);
        if (isR) {
            ctx.font = `${Math.round(fs * 0.85)}px monospace`;
            ctx.fillStyle = "rgba(124,134,200,0.55)";
            ctx.fillText("router", x, y + rad + 4 + fs + 2);
        }
    }
    function drawPacket(p) {
        if (p.phase === "wait" || p.phase === "die")
            return;
        let x;
        let y;
        if (p.phase === "go") {
            const t = ease(p.t);
            x = nx(p.src) + (nx(hub) - nx(p.src)) * t;
            y = ny(p.src) + (ny(hub) - ny(p.src)) * t;
        }
        else {
            const t = ease(p.t);
            x = nx(hub) + (nx(p.dst) - nx(hub)) * t;
            y = ny(hub) + (ny(p.dst) - ny(hub)) * t;
        }
        const a = Math.min(1, p.opacity);
        const halo = ctx.createRadialGradient(x, y, 0, x, y, 10);
        halo.addColorStop(0, `rgba(${p.r},${p.g},${p.b},${0.3 * a})`);
        halo.addColorStop(1, `rgba(${p.r},${p.g},${p.b},0)`);
        ctx.beginPath();
        ctx.arc(x, y, 10, 0, Math.PI * 2);
        ctx.fillStyle = halo;
        ctx.fill();
        ctx.beginPath();
        ctx.arc(x, y, 3, 0, Math.PI * 2);
        ctx.fillStyle = `rgba(${p.r},${p.g},${p.b},${a})`;
        ctx.fill();
    }
    function frame(ts) {
        rafId = requestAnimationFrame(frame);
        if (cw === 0 || ch === 0)
            return;
        const dt = Math.min((ts - prevTs) / 1000, 0.05);
        prevTs = ts;
        if (ts >= nextSpawn)
            spawn(ts);
        update(dt);
        ctx.clearRect(0, 0, cw, ch);
        drawLines();
        drawColumnLabels();
        for (const n of allNodes)
            drawNode(n);
        for (const p of packets)
            drawPacket(p);
    }
    document.addEventListener("visibilitychange", () => {
        if (document.hidden) {
            cancelAnimationFrame(rafId);
        }
        else {
            prevTs = performance.now();
            rafId = requestAnimationFrame(frame);
        }
    });
    prevTs = performance.now();
    rafId = requestAnimationFrame(frame);
})();
