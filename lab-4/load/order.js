// Нагрузка на путь заказа: POST /order, то есть api → postgres.
//
// Исполнитель constant-arrival-rate — «открытая» модель: k6 отправляет RATE запросов
// в секунду по часам, не дожидаясь ответов, как живые покупатели. Если сервис
// тормозит, это видно по задержкам, а не прячется за тем, что клиент сам притормозил.
//
//   k6 run lab-4/load/order.js                               # 50 rps, 15 минут
//   k6 run -e RATE=20 -e DURATION=2m lab-4/load/order.js     # другие частота и длительность
import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:30080';

export const options = {
  scenarios: {
    orders: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 50),
      timeUnit: '1s',
      duration: __ENV.DURATION || '15m',
      // виртуальные пользователи нужны на запросы «в полёте»: при 50 rps и ответе за 20 мс
      // хватает одного, запас на случай, когда сервис затормозит
      preAllocatedVUs: 20,
      maxVUs: 200,
    },
  },
  // SLA (часть 8): 95% заказов быстрее 250 мс и меньше 1% ошибок, даже под перегрузкой.
  // 250 мс, а не 300: такая граница есть в гистограмме api, и на дашборде долю заказов
  // быстрее неё можно посчитать точно, а не приближённо. Не выполнено — k6 печатает ✗.
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<250'],
  },
};

const items = ['book', 'pen', 'mug', 'lamp', 'chair'];

export default function () {
  const body = JSON.stringify({
    item: items[Math.floor(Math.random() * items.length)],
    qty: 1 + Math.floor(Math.random() * 3),
  });
  const res = http.post(`${BASE_URL}/order`, body, {
    headers: { 'Content-Type': 'application/json' },
  });
  check(res, { 'заказ создан (201)': (r) => r.status === 201 });
}
