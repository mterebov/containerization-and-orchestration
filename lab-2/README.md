# Лаба 2 — Мониторинг всякого важного

## Часть 0 — свой сервис (сервис Клода)

С помощью клодик кода был сгенерирован сервис на Go, выполняющий все требования из лабы. Ручки:

- `GET /health`
- `GET /fail`
- `GET /slow`
- `GET /load`
- `GET /metrics`

Что сервис отдаёт:

**Метрики:**

- `http_requests_total{method,route,code}`;
- `http_request_errors_total{route}` — только 5xx;
- `http_request_duration_seconds{method,route,code}`;
- метрики рантайма `go_*` и процесса `process_*`.

**Логи:** JSON в stdout.

**Трейсы:** корневой span (`GET /slow` и т. п.) создаёт otelhttp, внутри — вложенные `slow-op` и `fail-op`. Отправка по OTLP/HTTP на адрес из `OTEL_EXPORTER_OTLP_ENDPOINT`.

Далее установили helm и kind на макбук (Pro M5 14", 16/512 GB).

После этого подняли кластер через kind и проверили, что с ним всё ок:

![kind create cluster и kubectl get nodes](image.png)

Вроде всё действительно ок (нода была NotReady, но через минуту стала Ready), но мне посоветовали обновить kubectl, так как он у меня отставал на 3 версии. Вроде и так нормик, но на всякий обновил.

После этого собрали образ и загрузили его в ноду:

![docker build и kind load docker-image](image-1.png)

Он действительно подгрузился:

![crictl images на ноде: docker.io/library/api:0.1.0](image-2.png)

Написали манифест, в нём расписали, что делать, если под не готов или завис, и сколько ресурсов выделять. Он лежит в [k8s/api.yaml](k8s/api.yaml).

Применили манифест, перенаправили порты — и увидели, что `/health` отвечает на запросы, а значит вайбик:

![kubectl apply, rollout status, port-forward и curl /health](image-3.png)

Также посмотрели логи — по ним всё тоже вайб:

![логи подов api в JSON с trace_id](image-4.png)

## Часть 1 — Метрики (Prometheus + Grafana)

Написали файлик [monitoring/kube-prometheus-stack.yaml](monitoring/kube-prometheus-stack.yaml), в нём:

- выключили четыре компонента control plane (они отдают метрики только внутри ноды и поэтому висели бы недоступными);
- забираем метрики раз в 15 секунд;
- показали, где брать метрики api (ServiceMonitor);
- добавили дашборд Grafana, чтобы всё отрисовывалось.

На дашборде четыре графика:

- Rate — запросы в секунду по ручкам;
- Errors — доля ответов 5xx;
- Duration — p95 времени ответа, общий и по каждой ручке;
- запросы в секунду по подам.

Подключили репозиторий с чартами и установили стек:

![helm repo add и helm repo update](image-5.png)

![helm upgrade --install monitoring](image-6.png)

Пробросили порты и открыли Prometheus — у api 2/2 up, значит всё вайб:

![Prometheus, Target health: serviceMonitor/monitoring/api/0 — 2/2 up](image-7.png)

Посмотрели пароль для админки Grafana:

```bash
kubectl -n monitoring get secret monitoring-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

И зашли в неё:

![дашборд api — RED без нагрузки](image-10.png)

Выполняем следующую команду, нагружая сервис:

```bash
for i in {1..30}; do curl -s "localhost:8080/load?n=300" >/dev/null; sleep 2; done
```

И видим результат: нагрузка растёт только на один под (port-forward ведёт только в один под), `/health` значительно вырос, так как `/load` дёргает его много раз в секунду, p95 времени ответа для `/load` тоже увеличился:

![дашборд после /load: всплеск /health и p95 /load](image-11.png)

Далее вводим команду:

```bash
for i in {1..60}; do curl -s localhost:8080/fail >/dev/null; sleep 1; done
```

И видим, как возрастает процент ответов с ошибками:

![дашборд после /fail: доля 5xx до 60%](image-12.png)

После — команда:

```bash
for i in {1..20}; do curl -s localhost:8080/slow >/dev/null; done
```

Видим, что p95 времени ответа для `/slow` — примерно 2.9 секунды:

![дашборд после /slow: p95 /slow — 2.91 s](image-13.png)
