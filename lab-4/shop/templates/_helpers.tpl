{{/*
shop.spread: как разнести реплики одного Deployment по нодам. Механизм выбирается
в values (spread.mechanism), шаблон рендерит поле spec пода под этот механизм.

  {{- include "shop.spread" (dict "app" "api" "spread" .Values.api.spread) | nindent 6 }}

matchLabelKeys: [pod-template-hash] в обоих вариантах: при раскатке считаются только
поды той же ревизии (ReplicaSet). Без этого новый под при maxSurge: 1 учитывает и старые
поды: у podAntiAffinity ему некуда встать и раскатка зависает, а у
topologySpreadConstraints он может лечь к своей же новой реплике, и после ухода
старых подов обе окажутся на одной ноде.
*/}}
{{- define "shop.spread" -}}
{{- $s := .spread -}}
{{- if eq $s.mechanism "topologySpreadConstraints" -}}
topologySpreadConstraints:
  - maxSkew: {{ $s.maxSkew }}
    topologyKey: {{ $s.topologyKey }}
    # DoNotSchedule: лучше Pending, чем перекос; ScheduleAnyway: перекос допустим, если иначе некуда
    whenUnsatisfiable: {{ ternary "DoNotSchedule" "ScheduleAnyway" $s.hard }}
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: {{ .app }}
    matchLabelKeys:
      - pod-template-hash
    # нода с taint, который под не терпит (например memory-pressure), не считается доменом:
    # иначе баланс требовал бы поставить реплику туда, куда её всё равно не пустят
    nodeTaintsPolicy: Honor
{{- else if eq $s.mechanism "podAntiAffinity" -}}
affinity:
  podAntiAffinity:
  {{- if $s.hard }}
    requiredDuringSchedulingIgnoredDuringExecution:
      - topologyKey: {{ $s.topologyKey }}
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: {{ .app }}
        matchLabelKeys:
          - pod-template-hash
  {{- else }}
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          topologyKey: {{ $s.topologyKey }}
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: {{ .app }}
          matchLabelKeys:
            - pod-template-hash
  {{- end }}
{{- else -}}
{{- fail (printf "%s.spread.mechanism: topologySpreadConstraints или podAntiAffinity, а не %q" .app $s.mechanism) -}}
{{- end -}}
{{- end -}}
