/* Cronsole interface behaviour.
 *
 * Deliberately small and dependency free beyond what the page already loads.
 * Everything here is presentation: no decision that matters is taken in the
 * browser, so a script that fails leaves a working, if plainer, screen.
 */

(function () {
  'use strict';

  /* ---- theme ---------------------------------------------------------- */

  var THEME_KEY = 'cj-theme';

  /* Only an explicit choice is written down. Persisting the default too would
   * freeze it in every browser that ever loaded the page, so changing the
   * default later would reach nobody. */
  function applyTheme(theme, remember) {
    document.documentElement.setAttribute('data-bs-theme', theme);
    if (remember) {
      try { localStorage.setItem(THEME_KEY, theme); } catch (e) { /* private mode */ }
    }
  }

  function initTheme() {
    var stored = null;
    try { stored = localStorage.getItem(THEME_KEY); } catch (e) { /* private mode */ }
    /* Dark by default, whatever the operating system asks for. This is a
     * console that sits open on a screen all day, and the document already
     * carries data-bs-theme="dark" so there is no flash before this runs.
     * A light preference is honoured once somebody chooses it. */
    applyTheme(stored === 'light' ? 'light' : 'dark', false);

    var toggle = document.getElementById('themeToggle');
    if (toggle) {
      toggle.addEventListener('click', function () {
        var current = document.documentElement.getAttribute('data-bs-theme');
        applyTheme(current === 'dark' ? 'light' : 'dark', true);
        /* The charts take their colours from the theme's CSS variables, so
         * they have to be rebuilt rather than restyled. Both are called
         * unconditionally; each is a no-op on a page that does not have it. */
        window.renderActivity();
        window.renderJobTrend();
      });
    }
  }

  /* ---- toasts --------------------------------------------------------- */

  window.toast = function (message, kind) {
    var host = document.getElementById('toasts');
    if (!host) { return; }
    var el = document.createElement('div');
    el.className = 'toast align-items-center border-0 text-bg-' + (kind || 'secondary');
    el.setAttribute('role', 'alert');
    el.innerHTML = '<div class="d-flex"><div class="toast-body"></div>' +
      '<button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast"></button></div>';
    el.querySelector('.toast-body').textContent = message;
    host.appendChild(el);
    var toast = new bootstrap.Toast(el, { delay: 5000 });
    toast.show();
    el.addEventListener('hidden.bs.toast', function () { el.remove(); });
  };

  /* HTMX reports failures through the response body. Surfacing them as a
   * toast is the difference between "the button did nothing" and knowing
   * exactly which rule refused. */
  document.body.addEventListener('htmx:responseError', function (event) {
    var message = label('requestFailed');
    var body = event.detail.xhr.responseText || '';
    try {
      var parsed = JSON.parse(body);
      if (parsed && parsed.message) { message = parsed.message; }
    } catch (e) {
      /* Not JSON. A handler that fails renders a whole HTML page as its 500,
       * and putting that in a toast fills the screen with markup. Only a short,
       * plain body is worth showing; anything else keeps the generic message. */
      var plain = body.trim();
      if (plain && plain.length < 200 && plain.indexOf('<') === -1) { message = plain; }
    }
    window.toast(message, 'danger');
  });

  document.body.addEventListener('htmx:sendError', function () {
    window.toast(label('unreachable'), 'danger');
  });

  /* ---- charts ---------------------------------------------------------- */

  /* Chart labels come from the server rather than from a copy of the
   * catalogue in here. The templates already render the status words, so this
   * reads them out of the page and there is exactly one place a translation
   * lives. Falls back to the key, which is the English word anyway. */
  function label(key) {
    var host = document.getElementById('chartLabels');
    if (!host) { return key; }
    try {
      var labels = JSON.parse(host.textContent);
      return labels[key] || key;
    } catch (e) {
      return key;
    }
  }

  /* ---- painting a chart ------------------------------------------------ */

  /* Every chart is built through here, on the next frame, and never twice for
   * the same canvas in one frame.
   *
   * Two faults on the dashboard made this necessary, and both showed up as the
   * same thing: a blurred chart drawn several times too large.
   *
   *  - htmx swaps the body of #dashboard every thirty seconds, which replaces
   *    the canvas NODE. A chart held in a variable is then bound to a node that
   *    is no longer on the page, so destroying it leaves the live canvas with
   *    whatever was drawn on it. Chart.getChart is asked what is ACTUALLY
   *    attached to the element in the document instead of trusting a variable.
   *
   *  - Chart.js sizes its backing store from the parent's box when it is
   *    constructed. Built against a box that has not been laid out, it makes a
   *    tiny bitmap which the browser then stretches over the real element. That
   *    is the blur. Waiting for the box to have a size is the fix; drawing
   *    something wrong and letting a later resize correct it is not, because
   *    nothing resizes on a page that only refreshes its data. */

  var paintFrames = {};

  /* Roughly half a second at 60fps. A canvas on a hidden tab never gets a box,
   * and retrying for the life of the page would be a spin. */
  var maxPaintAttempts = 30;

  function paintChart(canvasID, build) {
    if (paintFrames[canvasID]) {
      cancelAnimationFrame(paintFrames[canvasID]);
      paintFrames[canvasID] = 0;
    }

    var attempts = 0;
    function attempt() {
      paintFrames[canvasID] = 0;

      var canvas = document.getElementById(canvasID);
      if (!canvas || typeof Chart === 'undefined') { return; }

      var box = canvas.parentElement;
      if (!box || !box.clientWidth || !box.clientHeight) {
        if (++attempts < maxPaintAttempts) {
          paintFrames[canvasID] = requestAnimationFrame(attempt);
        }
        return;
      }

      var existing = Chart.getChart(canvas);
      if (existing) { existing.destroy(); }
      build(canvas);
    }

    paintFrames[canvasID] = requestAnimationFrame(attempt);
  }

  /* Chart colours come from the theme's CSS variables, read at paint time so a
   * theme switch repaints in the new palette. */
  function themeColour(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return v ? v.trim() : fallback;
  }

  /* ---- activity chart ------------------------------------------------- */

  window.renderActivity = function () {
    paintChart('activityChart', function (canvas) {
      /* Read inside the paint, not before it: a refresh that lands while the
       * frame is pending would otherwise draw the previous response. */
      var holder = document.getElementById('activityData');
      if (!holder) { return; }

      var buckets;
      try { buckets = JSON.parse(holder.textContent); } catch (e) { return; }
      if (!buckets) { buckets = []; }

      var labels = buckets.map(function (b) {
        var d = new Date(b.hour);
        return String(d.getHours()).padStart(2, '0') + ':00';
      });

      var colour = themeColour;

      new Chart(canvas, {
        type: 'bar',
        data: {
          labels: labels,
          datasets: [
            { label: label('success'), data: buckets.map(function (b) { return b.success; }), backgroundColor: colour('--bs-success', '#198754') },
            { label: label('failed'), data: buckets.map(function (b) { return b.failed; }), backgroundColor: colour('--bs-danger', '#dc3545') },
            { label: label('timeout'), data: buckets.map(function (b) { return b.timeout; }), backgroundColor: colour('--bs-warning', '#ffc107') },
            { label: label('skipped'), data: buckets.map(function (b) { return b.skipped; }), backgroundColor: colour('--bs-secondary', '#6c757d') }
          ]
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          /* The chart is redrawn every thirty seconds. Animating each redraw
           * makes a wall display flicker on a timer for no information. */
          animation: false,
          interaction: { mode: 'index', intersect: false },
          plugins: { legend: { position: 'bottom', labels: { boxWidth: 10, boxHeight: 10 } } },
          scales: {
            x: { stacked: true, grid: { display: false }, ticks: { maxTicksLimit: 24 } },
            y: { stacked: true, beginAtZero: true, ticks: { precision: 0 } }
          }
        }
      });

      var stamp = document.getElementById('refreshedAt');
      if (stamp) { stamp.textContent = label('updated') + ' ' + new Date().toLocaleTimeString(); }
    });
  };

  /* ---- job trend chart ------------------------------------------------- */

  /* One bar per run, oldest on the left, coloured by how the run ended. A run
   * that never executed has no duration; it is drawn as a full-height marker in
   * its status colour so a job that keeps skipping does not read as a gap. */

  window.renderJobTrend = function () {
    paintChart('jobTrendChart', function (canvas) {
    var holder = document.getElementById('jobTrendData');
    if (!holder) { return; }

    var points;
    try { points = JSON.parse(holder.textContent); } catch (e) { return; }
    if (!points || !points.length) { return; }

    var colour = themeColour;
    var palette = {
      success: colour('--bs-success', '#198754'),
      failed: colour('--bs-danger', '#dc3545'),
      timeout: colour('--bs-warning', '#ffc107'),
      skipped: colour('--bs-secondary', '#6c757d'),
      running: colour('--bs-primary', '#0d6efd')
    };

    /* A run with no duration still needs a bar. Giving it the tallest value on
     * the chart is what makes a wall of skips visible at a glance. */
    var longest = points.reduce(function (max, p) {
      return p.duration_ms > max ? p.duration_ms : max;
    }, 0) || 1;

    new Chart(canvas, {
      type: 'bar',
      data: {
        labels: points.map(function (p) { return new Date(p.at).toLocaleString(); }),
        datasets: [{
          label: label('duration'),
          data: points.map(function (p) {
            return p.duration_ms > 0 ? p.duration_ms : longest;
          }),
          backgroundColor: points.map(function (p) {
            return palette[p.status] || palette.skipped;
          }),
          borderWidth: 0,
          borderRadius: 2
        }]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
          tooltip: {
            callbacks: {
              title: function (items) { return points[items[0].dataIndex].status; },
              label: function (item) {
                var p = points[item.dataIndex];
                var parts = [new Date(p.at).toLocaleString()];
                parts.push(p.duration_ms > 0 ? p.duration_ms + ' ms' : 'did not run');
                if (p.http_status) { parts.push('HTTP ' + p.http_status); }
                return parts;
              }
            }
          }
        },
        scales: {
          x: { display: false, grid: { display: false } },
          y: {
            beginAtZero: true,
            grid: { color: colour('--cj-line-soft', '#eef2f7') },
            ticks: { precision: 0, callback: function (v) { return v + ' ms'; } }
          }
        },
        /* The bar is the target; clicking it opens the run it stands for. */
        onClick: function (_, elements) {
          if (!elements.length) { return; }
          var p = points[elements[0].index];
          if (p && p.run_id) { window.location.href = '/runs/' + p.run_id; }
        }
      }
    });
    canvas.style.cursor = 'pointer';
    });
  };

  /* ---- schedule editor ------------------------------------------------ */

  /* Rows are added client side so a job with four expressions is one save,
   * not four. The preview beside each row is fetched from the server, because
   * the server owns the cron grammar and a second implementation in the
   * browser would eventually disagree with it. */

  var previewTimers = {};

  function previewFor(input) {
    var row = input.closest('.schedule-row');
    if (!row) { return; }
    var line = row.querySelector('.preview-line');
    if (!line) { return; }

    var expression = input.value.trim();
    if (!expression) {
      line.textContent = '';
      line.classList.remove('invalid');
      return;
    }

    var key = input.dataset.previewKey || (input.dataset.previewKey = String(Date.now() + Math.random()));
    clearTimeout(previewTimers[key]);
    previewTimers[key] = setTimeout(function () {
      fetch('/jobs/schedule-preview?expression=' + encodeURIComponent(expression), {
        headers: { 'Accept': 'application/json' }
      })
        .then(function (r) { return r.json(); })
        .then(function (body) {
          var data = body && body.data ? body.data : body;
          if (!data || !data.valid) {
            line.classList.add('invalid');
            line.textContent = (data && data.error) ? data.error : 'not a valid expression';
            return;
          }
          line.classList.remove('invalid');
          var next = (data.next_runs || []).slice(0, 3).map(function (t) {
            var d = new Date(t);
            return d.toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit' });
          });
          line.textContent = data.description + (next.length ? ' — next: ' + next.join(', ') : '');
        })
        .catch(function () {
          line.textContent = '';
        });
    }, 300);
  }

  function bindScheduleRow(row) {
    var input = row.querySelector('input[name="schedules[]"]');
    if (!input) { return; }
    input.addEventListener('input', function () { previewFor(input); });
    if (input.value.trim()) { previewFor(input); }

    var remove = row.querySelector('.schedule-remove');
    if (remove) {
      remove.addEventListener('click', function () {
        var list = row.parentElement;
        row.remove();
        /* An empty list would submit a job with no schedule, which cannot be
         * activated. Leaving one blank row makes that obvious. */
        if (list && list.querySelectorAll('.schedule-row').length === 0) {
          addScheduleRow('');
        }
      });
    }
  }

  window.addScheduleRow = function (expression) {
    var list = document.getElementById('scheduleList');
    var template = document.getElementById('scheduleRowTemplate');
    if (!list || !template) { return; }

    var row = template.content.firstElementChild.cloneNode(true);
    var input = row.querySelector('input[name="schedules[]"]');
    if (input && expression) { input.value = expression; }
    list.appendChild(row);
    bindScheduleRow(row);
    if (input) { input.focus(); }
  };

  window.addHeaderRow = function () {
    var list = document.getElementById('headerList');
    var template = document.getElementById('headerRowTemplate');
    if (!list || !template) { return; }
    var row = template.content.firstElementChild.cloneNode(true);
    list.appendChild(row);
    bindHeaderRow(row);
  };

  function bindHeaderRow(row) {
    var remove = row.querySelector('.header-remove');
    if (remove) {
      remove.addEventListener('click', function () { row.remove(); });
    }
  }

  function initJobForm() {
    document.querySelectorAll('#scheduleList .schedule-row').forEach(bindScheduleRow);
    document.querySelectorAll('#headerList .header-row').forEach(bindHeaderRow);

    document.querySelectorAll('.preset-chip').forEach(function (chip) {
      chip.addEventListener('click', function () {
        var expression = chip.dataset.expression;
        var list = document.getElementById('scheduleList');
        if (!list) { return; }
        /* An empty first row is filled rather than pushed down: clicking a
         * preset on a new job should produce one schedule, not two. */
        var blank = Array.prototype.find.call(
          list.querySelectorAll('input[name="schedules[]"]'),
          function (input) { return !input.value.trim(); }
        );
        if (blank) {
          blank.value = expression;
          previewFor(blank);
        } else {
          window.addScheduleRow(expression);
        }
      });
    });

    /* The code field is derived from the name until the operator edits it
     * themselves, at which point it is left alone. */
    var name = document.getElementById('jobName');
    var code = document.getElementById('jobCode');
    if (name && code && !code.readOnly) {
      code.addEventListener('input', function () { code.dataset.touched = '1'; });
      name.addEventListener('input', function () {
        if (code.dataset.touched) { return; }
        code.value = name.value
          .toLowerCase()
          .replace(/[^a-z0-9_]+/g, '-')
          .replace(/^-+|-+$/g, '')
          .slice(0, 64);
      });
    }
  }

  /* ---- confirmation ---------------------------------------------------- */

  /* htmx fires htmx:confirm before every request, whether or not the element
   * carries hx-confirm. Cancelling it here and re-issuing after our own dialog
   * replaces window.confirm everywhere at once: the wording stays on the
   * action, in the template, and nothing has to be rewritten per screen.
   *
   * A browser confirm also blocks the whole tab, which on a screen that
   * refreshes itself means the dashboard stops updating behind the dialog. */
  document.body.addEventListener('htmx:confirm', function (event) {
    var question = event.detail.question;
    if (!question) { return; }

    event.preventDefault();
    askConfirm(question, event.detail.elt).then(function (accepted) {
      /* true skips the confirm on the way back through, which is what stops
       * this from asking a second time. */
      if (accepted) { event.detail.issueRequest(true); }
    });
  });

  /* askConfirm resolves true when the person accepts. It never rejects: a
   * dismissed dialog is an answer, not an error. */
  function askConfirm(question, source) {
    return new Promise(function (resolve) {
      var el = document.getElementById('confirmModal');
      if (!el || !window.bootstrap) {
        /* No modal on this page, or Bootstrap did not load. Falling back to the
         * browser dialog is worse than the modal and far better than performing
         * a delete nobody confirmed. */
        resolve(window.confirm(question));
        return;
      }

      /* The first sentence is the question, the rest is the consequence. Both
       * are already written that way in the templates. */
      var split = question.indexOf('? ');
      var title = split === -1 ? question : question.slice(0, split + 1);
      var detail = split === -1 ? '' : question.slice(split + 2);

      /* Destructive is read off the control that was clicked rather than from
       * the wording, so a rename of the copy cannot quietly turn the button
       * blue. */
      var destructive = !!(source && source.closest &&
        (source.closest('.btn-danger, .btn-outline-danger') ||
         source.querySelector('.btn-danger, .btn-outline-danger')));

      var accept = document.getElementById('confirmAccept');
      var icon = document.getElementById('confirmIcon');
      document.getElementById('confirmTitle').textContent = title;
      document.getElementById('confirmMessage').textContent = detail;
      accept.className = 'btn btn-sm ' + (destructive ? 'btn-danger' : 'btn-primary');
      accept.textContent = destructive ? label('yesDoIt') : label('confirm');
      icon.className = 'confirm-icon' + (destructive ? ' confirm-icon-danger' : '');
      icon.innerHTML = destructive
        ? '<i class="bi bi-exclamation-triangle"></i>'
        : '<i class="bi bi-question-lg"></i>';

      var modal = window.bootstrap.Modal.getOrCreateInstance(el);
      var answered = false;

      function onAccept() {
        answered = true;
        modal.hide();
      }
      function onHidden() {
        accept.removeEventListener('click', onAccept);
        el.removeEventListener('hidden.bs.modal', onHidden);
        resolve(answered);
      }

      accept.addEventListener('click', onAccept);
      el.addEventListener('hidden.bs.modal', onHidden);
      modal.show();
      /* Focus the safe option: a dialog that opens with the destructive button
       * focused turns a stray Enter into a delete. */
      el.addEventListener('shown.bs.modal', function once() {
        el.removeEventListener('shown.bs.modal', once);
        var cancel = el.querySelector('[data-bs-dismiss="modal"]');
        if (cancel) { cancel.focus(); }
      });
    });
  }

  /* ---- row links ------------------------------------------------------- */

  /* The whole row navigates, not just the link text in its first cell. The
   * anchor stays in the markup because it is what makes the row reachable by
   * keyboard, middle-clickable and copyable; this only widens the target for a
   * plain left click.
   *
   * Delegated, so rows swapped in by htmx are covered without re-binding. */
  document.body.addEventListener('click', function (event) {
    var row = event.target.closest('[data-row-href]');
    if (!row) { return; }
    /* Anything the row already offers wins: the link itself, an action button,
     * a form control, a text selection the reader is making. */
    if (event.target.closest('a, button, input, select, textarea, label, [data-copy]')) { return; }
    if (window.getSelection && String(window.getSelection()).length > 0) { return; }

    var href = row.dataset.rowHref;
    if (!href) { return; }
    /* Cmd or Ctrl opens a new tab, matching what the anchor would have done. */
    if (event.metaKey || event.ctrlKey) {
      window.open(href, '_blank', 'noopener');
      return;
    }
    window.location.href = href;
  });

  /* ---- copy to clipboard ---------------------------------------------- */

  document.body.addEventListener('click', function (event) {
    var button = event.target.closest('[data-copy]');
    if (!button) { return; }
    var value = button.dataset.copy;
    if (!value) { return; }

    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(value).then(function () {
        window.toast('Copied.', 'success');
      });
      return;
    }
    /* Clipboard access needs a secure context; over plain HTTP on a laptop it
     * is unavailable, so fall back rather than failing silently. */
    var field = document.createElement('textarea');
    field.value = value;
    field.style.position = 'fixed';
    field.style.opacity = '0';
    document.body.appendChild(field);
    field.select();
    try { document.execCommand('copy'); window.toast('Copied.', 'success'); } catch (e) { /* ignore */ }
    field.remove();
  });

  /* ---- boot ----------------------------------------------------------- */

  function init() {
    initTheme();
    initJobForm();
    if (document.getElementById('activityChart')) { window.renderActivity(); }
    if (document.getElementById('jobTrendChart')) { window.renderJobTrend(); }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
