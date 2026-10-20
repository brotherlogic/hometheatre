package webui

const dashboardTemplateHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>HomeTheatre — DVD ISO Ripper &amp; Kodi NFS Pipeline</title>
  <style>
    :root {
      --bg: #0f172a;
      --panel: #1e293b;
      --border: #334155;
      --text: #f8fafc;
      --muted: #94a3b8;
      --accent: #38bdf8;
      --success: #22c55e;
      --warn: #f59e0b;
      --danger: #ef4444;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 24px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
      line-height: 1.5;
    }
    .container { max-width: 1160px; margin: 0 auto; }
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 24px;
      padding-bottom: 16px;
      border-bottom: 1px solid var(--border);
    }
    h1 { margin: 0; font-size: 1.5rem; }
    h2 { margin-top: 0; font-size: 1.2rem; color: var(--accent); }
    .subtitle { color: var(--muted); font-size: 0.9rem; }
    .alert {
      padding: 12px 16px;
      border-radius: 8px;
      margin-bottom: 16px;
      font-weight: 500;
    }
    .alert-success { background: rgba(34, 197, 94, 0.15); border: 1px solid var(--success); color: #86efac; }
    .alert-error { background: rgba(239, 68, 68, 0.15); border: 1px solid var(--danger); color: #fca5a5; }
    .alert-warn { background: rgba(245, 158, 11, 0.15); border: 1px solid var(--warn); color: #fcd34d; }
    .status-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
      gap: 16px;
      margin-bottom: 24px;
    }
    .card {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 10px;
      padding: 18px;
      margin-bottom: 24px;
    }
    .status-card { margin-bottom: 0; }
    .status-label { color: var(--muted); font-size: 0.8rem; text-transform: uppercase; letter-spacing: 0.05em; }
    .status-value { font-size: 1.25rem; font-weight: 700; margin-top: 6px; }
    .status-sub { color: var(--muted); font-size: 0.85rem; margin-top: 4px; }
    .badge {
      display: inline-block;
      padding: 2px 8px;
      border-radius: 999px;
      font-size: 0.78rem;
      font-weight: 600;
      background: #334155;
      color: var(--text);
    }
    .badge-completed { background: rgba(34, 197, 94, 0.2); color: #86efac; }
    .badge-unidentified { background: rgba(245, 158, 11, 0.2); color: #fcd34d; }
    .badge-failed { background: rgba(239, 68, 68, 0.2); color: #fca5a5; }
    .unid-item {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 16px;
      margin-bottom: 16px;
      background: rgba(15, 23, 42, 0.5);
    }
    .form-row {
      display: flex;
      flex-wrap: wrap;
      gap: 12px;
      margin-top: 12px;
      align-items: flex-end;
    }
    .form-group {
      display: flex;
      flex-direction: column;
      gap: 4px;
    }
    label { font-size: 0.82rem; color: var(--muted); font-weight: 600; }
    input[type="text"], input[type="number"], select {
      background: #0f172a;
      border: 1px solid var(--border);
      color: var(--text);
      padding: 8px 10px;
      border-radius: 6px;
      font-size: 0.9rem;
    }
    .candidates-list {
      margin: 12px 0;
      display: flex;
      flex-direction: column;
      gap: 8px;
    }
    .candidate-option {
      display: flex;
      align-items: flex-start;
      gap: 10px;
      padding: 10px;
      border: 1px solid var(--border);
      border-radius: 6px;
      background: #0f172a;
      cursor: pointer;
    }
    .btn {
      padding: 8px 14px;
      border-radius: 6px;
      border: none;
      font-weight: 600;
      font-size: 0.9rem;
      cursor: pointer;
    }
    .btn-primary { background: var(--accent); color: #0f172a; }
    .btn-warn { background: var(--warn); color: #0f172a; }
    .btn-danger { background: var(--danger); color: #fff; }
    .btn-secondary { background: #334155; color: var(--text); }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.9rem;
    }
    th, td {
      text-align: left;
      padding: 10px 8px;
      border-bottom: 1px solid var(--border);
      vertical-align: top;
    }
    th { color: var(--muted); font-weight: 600; font-size: 0.8rem; text-transform: uppercase; }
    pre.logs {
      background: #090d16;
      border: 1px solid var(--border);
      border-radius: 6px;
      padding: 12px;
      overflow-x: auto;
      max-height: 280px;
      font-size: 0.8rem;
      color: #cbd5e1;
      margin: 0;
    }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 0.85rem; }
    .progress-container {
      margin-top: 12px;
    }
    .progress-header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      font-size: 0.82rem;
      color: var(--muted);
      margin-bottom: 6px;
      gap: 8px;
    }
    .progress-track {
      width: 100%;
      height: 10px;
      background: #0f172a;
      border: 1px solid var(--border);
      border-radius: 999px;
      overflow: hidden;
    }
    .progress-fill {
      height: 100%;
      background: linear-gradient(90deg, var(--accent), var(--success));
      border-radius: 999px;
      transition: width 0.3s ease;
    }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div>
        <h1>HomeTheatre DVD Ripper</h1>
        <div class="subtitle">1:1 Decrypted DVD ISO Backup &amp; Kodi NFS Library Pipeline</div>
      </div>
      <div>
        <a href="/" class="btn btn-secondary" style="text-decoration:none;">Refresh Status</a>
      </div>
    </header>

    {{if .FlashMsg}}
      <div class="alert alert-success">{{.FlashMsg}}</div>
    {{end}}
    {{if .ErrorMsg}}
      <div class="alert alert-error">{{.ErrorMsg}}</div>
    {{end}}

    <!-- Live Status Section -->
    <div class="status-grid">
      <div class="card status-card">
        <div class="status-label">Optical Drive &amp; Ripper State</div>
        <div class="status-value" id="live-drive-status">{{.Status.CombinedDriveStatus}}</div>
        <div class="status-sub">Drive: <span class="mono" id="live-drive-raw">{{.Status.DriveStatus}}</span> | Pipeline: <span class="mono" id="live-ripper-raw">{{.Status.RipperStatus}}</span></div>
        <div class="progress-container" id="rip-progress-container" style="{{if not .Status.RipActive}}display:none;{{end}}">
          <div class="progress-header">
            <span class="mono" id="rip-progress-label">{{.Status.RipLabel}}</span>
            <span class="mono" id="rip-progress-text">{{printf "%.1f%%" .Status.RipProgressPct}} ({{formatGiB .Status.RipBytesDone}} / {{formatGiB .Status.RipBytesTotal}})</span>
          </div>
          <div class="progress-track" id="rip-progress-track" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="{{printf "%.1f" .Status.RipProgressPct}}">
            <div class="progress-fill" id="rip-progress-bar" style="width: {{printf "%.1f%%" .Status.RipProgressPct}};"></div>
          </div>
        </div>
      </div>

      <div class="card status-card">
        <div class="status-label">Background NFS Transfer Queue</div>
        <div class="status-value" id="live-nfs-status">{{.Status.NFSStatus}}</div>
        <div class="status-sub">Queued Transfers: <span class="mono" id="live-pending-transfers">{{.Status.PendingTransfers}}</span></div>
        {{if .Status.NFSRetryPending}}
          <form method="POST" action="/retry-transfer" style="margin-top:10px;">
            <button type="submit" id="retry-transfer-btn" class="btn btn-warn">Retry Transfer Now</button>
          </form>
        {{else}}
          <form method="POST" action="/retry-transfer" style="margin-top:10px;">
            <button type="submit" id="retry-transfer-btn" class="btn btn-secondary">Retry Transfer Now</button>
          </form>
        {{end}}
      </div>

      <div class="card status-card">
        <div class="status-label">Unidentified / Needs Tagging Queue</div>
        <div class="status-value" id="live-unidentified-count">{{.Status.UnidentifiedCount}} Disc(s)</div>
        <div class="status-sub">Backed statelessly by <span class="mono">/mnt/nfs/unidentified/*.probe.json</span></div>
      </div>
    </div>

    <!-- Unidentified Tagging Queue Section -->
    <div class="card" id="unidentified-queue">
      <h2>Unidentified / Needs Tagging Queue ({{len .Unidentified}})</h2>
      {{if not .Unidentified}}
        <p class="subtitle">No unidentified discs waiting in <span class="mono">/mnt/nfs/unidentified</span>.</p>
      {{else}}
        {{range $idx, $item := .Unidentified}}
          <div class="unid-item" data-iso="{{$item.Sidecar.ISOFilename}}">
            <div style="display:flex; justify-content:space-between; flex-wrap:wrap; gap:8px;">
              <div>
                <strong>{{$item.Sidecar.RawLabel}}</strong>
                <span class="mono" style="color:var(--muted);">({{$item.Sidecar.ISOFilename}}{{if $item.ISOSizeDisplay}}, {{$item.ISOSizeDisplay}}{{end}})</span>
              </div>
              <div>
                <span class="badge badge-unidentified">{{$item.Sidecar.Classification}}</span>
                {{if $item.Sidecar.Reason}}
                  <span class="badge">{{$item.Sidecar.Reason}}</span>
                {{end}}
              </div>
            </div>

            <div class="status-sub" style="margin-top:6px;">
              Probed: <span class="mono">{{$item.Sidecar.ProbedAt}}</span> |
              Tracks ({{$item.TrackCount}}): <span class="mono">{{$item.TracksSummary}}</span>
              {{if gt $item.Sidecar.Hints.EstimatedEpisodes 0}}
                | Estimated Episodes: <strong>{{$item.Sidecar.Hints.EstimatedEpisodes}}</strong> (Suggested: <span class="mono">{{$item.SuggestedSxxExx}}</span>)
              {{end}}
            </div>

            {{if $item.IsCollision}}
              <div class="alert alert-warn" style="margin-top:12px; margin-bottom:12px;">
                <strong>Target Collision Detected (<span class="mono">target_already_exists</span>):</strong>
                An ISO for this movie already exists on the NAS. Choose one of the multi-disc resolution options below:
                save with an <strong>Edition / Disc Suffix</strong> (e.g. <span class="mono">Disc 2</span> or <span class="mono">Bonus Features</span>),
                <strong>Replace Existing .iso</strong>, or <strong>Delete Unidentified .iso</strong>.
              </div>
            {{end}}

            <form method="POST" action="/unidentified/tag" id="form-{{$idx}}">
              <input type="hidden" name="iso_filename" value="{{$item.Sidecar.ISOFilename}}">

              <!-- Pre-filled TMDB Candidates -->
              {{if $item.Sidecar.TMDBCandidates}}
                <div class="candidates-list" id="candidates-{{$idx}}">
                  <label>Pre-Filled TMDB Candidates (Select Match):</label>
                  {{range $cIdx, $cand := $item.Sidecar.TMDBCandidates}}
                    <label class="candidate-option">
                      <input type="radio" name="candidate_radio_{{$idx}}"
                        {{if eq $cIdx 0}}checked{{end}}
                        onclick="selectCandidate({{$idx}}, {{$cand.ID}}, '{{js $cand.Title}}', {{$cand.Year}}, '{{js $cand.MediaType}}')">
                      <div>
                        <div>
                          <strong>{{$cand.Title}}</strong>
                          {{if gt $cand.Year 0}}({{$cand.Year}}){{end}}
                          <span class="badge">{{$cand.MediaType}}</span>
                          <span class="mono" style="color:var(--muted);">TMDB #{{$cand.ID}}</span>
                        </div>
                        {{if $cand.Overview}}
                          <div class="subtitle" style="margin-top:2px;">{{$cand.Overview}}</div>
                        {{end}}
                      </div>
                    </label>
                  {{end}}
                </div>
              {{end}}

              <!-- Interactive TMDB Search Box -->
              <div class="form-row">
                <div class="form-group" style="flex:1; min-width:200px;">
                  <label>Search TMDB (Optional Override)</label>
                  <div style="display:flex; gap:6px;">
                    <input type="text" id="search-q-{{$idx}}" value="{{$item.Sidecar.SanitizedQuery}}" placeholder="Search Movie or TV Show..." style="flex:1;">
                    <button type="button" class="btn btn-secondary" onclick="searchTMDB({{$idx}})">Search TMDB</button>
                  </div>
                </div>
              </div>
              <div id="search-results-{{$idx}}" class="candidates-list" style="display:none;"></div>

              <div class="form-row">
                <div class="form-group">
                  <label for="media-type-{{$idx}}">Media Type</label>
                  <select name="media_type" id="media-type-{{$idx}}" onchange="toggleMediaFields({{$idx}})">
                    <option value="movie" {{if eq $item.DefaultMediaType "movie"}}selected{{end}}>Movie</option>
                    <option value="tv" {{if eq $item.DefaultMediaType "tv"}}selected{{end}}>TV Show</option>
                  </select>
                </div>

                <div class="form-group">
                  <label for="tmdb-id-{{$idx}}">TMDB ID</label>
                  <input type="number" name="tmdb_id" id="tmdb-id-{{$idx}}" value="{{$item.DefaultTMDBID}}" style="width:110px;">
                </div>

                <div class="form-group" style="flex:1; min-width:180px;">
                  <label for="title-{{$idx}}">Title</label>
                  <input type="text" name="title" id="title-{{$idx}}" value="{{$item.DefaultTitle}}">
                </div>

                <div class="form-group">
                  <label for="year-{{$idx}}">Year</label>
                  <input type="number" name="year" id="year-{{$idx}}" value="{{$item.DefaultYear}}" style="width:95px;">
                </div>
              </div>

              <!-- TV Show Season & Episode Picker (SxxExx) -->
              <div class="form-row" id="tv-fields-{{$idx}}" style="{{if ne $item.DefaultMediaType "tv"}}display:none;{{end}}">
                <div class="form-group">
                  <label for="season-{{$idx}}">Season</label>
                  <input type="number" name="season" id="season-{{$idx}}" value="{{$item.DefaultSeason}}" min="1" style="width:85px;"
                    oninput="updateSxxExxPreview({{$idx}}, {{$item.Sidecar.Hints.EstimatedEpisodes}})">
                </div>

                <div class="form-group">
                  <label for="start-ep-{{$idx}}">Start Episode</label>
                  <input type="number" name="start_episode" id="start-ep-{{$idx}}" value="{{$item.DefaultStartEp}}" min="1" style="width:110px;"
                    oninput="onStartEpChange({{$idx}}, {{$item.Sidecar.Hints.EstimatedEpisodes}})">
                </div>

                <div class="form-group">
                  <label for="end-ep-{{$idx}}">End Episode</label>
                  <input type="number" name="end_episode" id="end-ep-{{$idx}}" value="{{$item.DefaultEndEp}}" min="1" style="width:110px;"
                    oninput="updateSxxExxPreview({{$idx}}, {{$item.Sidecar.Hints.EstimatedEpisodes}})">
                </div>

                <div class="form-group">
                  <label>Kodi Multi-Episode Tag</label>
                  <div class="badge mono" id="sxxexx-preview-{{$idx}}" style="padding:8px 12px; font-size:0.9rem;">{{$item.SuggestedSxxExx}}</div>
                </div>
              </div>

              <!-- Movie Edition / Disc Suffix (Multi-Disc Resolution) -->
              <div class="form-row" id="movie-fields-{{$idx}}" style="{{if eq $item.DefaultMediaType "tv"}}display:none;{{end}}">
                <div class="form-group" style="min-width:260px;">
                  <label for="edition-{{$idx}}">Edition / Disc Suffix (Optional, e.g. Disc 2, Bonus Features)</label>
                  <input type="text" name="edition_suffix" id="edition-{{$idx}}" value="{{$item.DefaultEdition}}" placeholder="Disc 2 / Bonus Features / Director's Cut">
                </div>
              </div>

              <!-- 1-Click Confirmation & Multi-Disc Collision Actions -->
              <div class="form-row" style="margin-top:16px;">
                <button type="submit" name="action" value="tag" class="btn btn-primary">
                  Confirm &amp; Move to NFS (1-Click)
                </button>
                <button type="submit" name="action" value="replace" class="btn btn-warn">
                  Replace Existing .iso
                </button>
                <button type="submit" name="action" value="delete" class="btn btn-danger">
                  Delete Unidentified .iso
                </button>
              </div>
            </form>
          </div>
        {{end}}
      {{end}}
    </div>

    <!-- Persistent Rip History Section (<SCRATCH_DIR>/history.json) -->
    <div class="card" id="rip-history">
      <h2>Recent Rip History ({{len .History}})</h2>
      {{if not .History}}
        <p class="subtitle">No rip history entries recorded yet in <span class="mono">history.json</span>.</p>
      {{else}}
        <table>
          <thead>
            <tr>
              <th>Timestamp</th>
              <th>Volume Label</th>
              <th>Title / Media Type</th>
              <th>Stage</th>
              <th>Status</th>
              <th>NFS Path / Details</th>
            </tr>
          </thead>
          <tbody>
            {{range .History}}
              <tr>
                <td class="mono">{{.Timestamp}}</td>
                <td class="mono">{{.RawLabel}}</td>
                <td>
                  {{if .Title}}<strong>{{.Title}}</strong><br>{{end}}
                  <span class="subtitle">{{.Classification}}</span>
                </td>
                <td class="mono">
                  {{.Stage}}
                  {{if gt .RescuedRatio 0.0}}
                    <br><span class="subtitle">Rescued: {{printf "%.1f%%" (mulPct .RescuedRatio)}}</span>
                  {{end}}
                </td>
                <td>
                  <span class="badge badge-{{.Status}}">{{.Status}}</span>
                </td>
                <td class="mono">
                  {{if .ISOPath}}<div>{{.ISOPath}}</div>{{end}}
                  {{if .Reason}}<div class="subtitle">Reason: {{.Reason}}</div>{{end}}
                  {{if .Log}}<pre class="logs" style="margin-top:6px; max-height:90px;">{{.Log}}</pre>{{end}}
                </td>
              </tr>
            {{end}}
          </tbody>
        </table>
      {{end}}
    </div>

    <!-- Live Daemon Logs Section -->
    <div class="card" id="daemon-logs">
      <h2>Daemon Logs</h2>
      {{if not .Logs}}
        <p class="subtitle">No daemon log output buffered yet.</p>
      {{else}}
        <pre class="logs">{{range .Logs}}{{.}}
{{end}}</pre>
      {{end}}
    </div>
  </div>

  <script>
    function selectCandidate(idx, id, title, year, mediaType) {
      document.getElementById('tmdb-id-' + idx).value = id;
      document.getElementById('title-' + idx).value = title;
      if (year > 0) {
        document.getElementById('year-' + idx).value = year;
      }
      if (mediaType === 'tv' || mediaType === 'movie') {
        document.getElementById('media-type-' + idx).value = mediaType;
        toggleMediaFields(idx);
      }
    }

    function toggleMediaFields(idx) {
      var mt = document.getElementById('media-type-' + idx).value;
      var tvRow = document.getElementById('tv-fields-' + idx);
      var movieRow = document.getElementById('movie-fields-' + idx);
      if (mt === 'tv') {
        tvRow.style.display = 'flex';
        movieRow.style.display = 'none';
      } else {
        tvRow.style.display = 'none';
        movieRow.style.display = 'flex';
      }
    }

    function onStartEpChange(idx, estCount) {
      var startInput = document.getElementById('start-ep-' + idx);
      var endInput = document.getElementById('end-ep-' + idx);
      var startEp = parseInt(startInput.value, 10) || 1;
      var count = (estCount && estCount > 0) ? estCount : 1;
      endInput.value = startEp + count - 1;
      updateSxxExxPreview(idx, estCount);
    }

    function updateSxxExxPreview(idx, estCount) {
      var season = parseInt(document.getElementById('season-' + idx).value, 10) || 1;
      var startEp = parseInt(document.getElementById('start-ep-' + idx).value, 10) || 1;
      var endEp = parseInt(document.getElementById('end-ep-' + idx).value, 10) || startEp;
      if (endEp < startEp) endEp = startEp;
      var s = 'S' + (season < 10 ? '0' + season : season);
      for (var e = startEp; e <= endEp && e - startEp < 50; e++) {
        s += 'E' + (e < 10 ? '0' + e : e);
      }
      document.getElementById('sxxexx-preview-' + idx).textContent = s;
    }

    function searchTMDB(idx) {
      var q = document.getElementById('search-q-' + idx).value.trim();
      var mt = document.getElementById('media-type-' + idx).value;
      var container = document.getElementById('search-results-' + idx);
      if (!q) return;
      container.style.display = 'flex';
      container.innerHTML = '<div class="subtitle">Searching TMDB...</div>';
      fetch('/api/tmdb/search?q=' + encodeURIComponent(q) + '&type=' + encodeURIComponent(mt))
        .then(function(r) { return r.json(); })
        .then(function(data) {
          if (!data.candidates || data.candidates.length === 0) {
            container.innerHTML = '<div class="subtitle">No TMDB matches found.</div>';
            return;
          }
          var html = '<label>TMDB Search Results:</label>';
          data.candidates.forEach(function(c, cIdx) {
            html += '<label class="candidate-option">' +
              '<input type="radio" name="search_radio_' + idx + '" ' + (cIdx === 0 ? 'checked' : '') +
              ' onclick="selectCandidate(' + idx + ', ' + c.id + ', ' + JSON.stringify(c.title).replace(/"/g, '&quot;') + ', ' + (c.year || 0) + ', \'' + c.media_type + '\')">' +
              '<div><strong>' + c.title + '</strong> ' + (c.year ? '(' + c.year + ')' : '') +
              ' <span class="badge">' + c.media_type + '</span> <span class="mono" style="color:var(--muted);">TMDB #' + c.id + '</span></div>' +
              '</label>';
          });
          container.innerHTML = html;
          var first = data.candidates[0];
          selectCandidate(idx, first.id, first.title, first.year || 0, first.media_type);
        })
        .catch(function(err) {
          container.innerHTML = '<div class="subtitle">Search error: ' + err + '</div>';
        });
    }

    function formatGiB(bytes) {
      var gib = 1024 * 1024 * 1024;
      var val = (bytes && bytes > 0) ? (bytes / gib) : 0;
      return val.toFixed(2) + ' GiB';
    }

    function pollStatus() {
      fetch('/api/status')
        .then(function(r) { return r.json(); })
        .then(function(st) {
          var driveEl = document.getElementById('live-drive-status');
          if (driveEl && st.combined_drive_status) {
            driveEl.textContent = st.combined_drive_status;
          }
          var driveRawEl = document.getElementById('live-drive-raw');
          if (driveRawEl && st.drive_status) {
            driveRawEl.textContent = st.drive_status;
          }
          var ripperRawEl = document.getElementById('live-ripper-raw');
          if (ripperRawEl && st.ripper_status) {
            ripperRawEl.textContent = st.ripper_status;
          }
          var progContainer = document.getElementById('rip-progress-container');
          if (progContainer) {
            progContainer.style.display = st.rip_active ? '' : 'none';
          }
          var pct = (typeof st.rip_progress_pct === 'number' && st.rip_progress_pct > 0) ? st.rip_progress_pct : 0;
          if (pct > 100) pct = 100;
          var progLabel = document.getElementById('rip-progress-label');
          if (progLabel) {
            progLabel.textContent = st.rip_label || '';
          }
          var progText = document.getElementById('rip-progress-text');
          if (progText) {
            progText.textContent = pct.toFixed(1) + '% (' + formatGiB(st.rip_bytes_done) + ' / ' + formatGiB(st.rip_bytes_total) + ')';
          }
          var progBar = document.getElementById('rip-progress-bar');
          if (progBar) {
            progBar.style.width = pct.toFixed(1) + '%';
          }
          var progTrack = document.getElementById('rip-progress-track');
          if (progTrack) {
            progTrack.setAttribute('aria-valuenow', pct.toFixed(1));
          }
          var nfsEl = document.getElementById('live-nfs-status');
          if (nfsEl && st.nfs_status) {
            nfsEl.textContent = st.nfs_status;
          }
          var pendingEl = document.getElementById('live-pending-transfers');
          if (pendingEl && typeof st.pending_transfers === 'number') {
            pendingEl.textContent = String(st.pending_transfers);
          }
          var retryBtn = document.getElementById('retry-transfer-btn');
          if (retryBtn) {
            retryBtn.className = st.nfs_retry_pending ? 'btn btn-warn' : 'btn btn-secondary';
          }
          var unidEl = document.getElementById('live-unidentified-count');
          if (unidEl && typeof st.unidentified_count === 'number') {
            unidEl.textContent = st.unidentified_count + ' Disc(s)';
          }
        })
        .catch(function() {});
    }

    setInterval(pollStatus, 2000);
  </script>
</body>
</html>
`
