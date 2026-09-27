"""Аватарки двух ботов.

Рисуем, а не берём из стока: у ботов один продукт, и по картинкам их
должно быть видно как пару — одна геометрия, одна сетка, разный цвет и
разный знак. Знак отвечает на вопрос «чей это бот»: у креаторского —
кадр с точкой записи, у клиентского — растущая линия просмотров.

512×512: столько просит Telegram, меньше — и в списке чатов аватар
мылит.
"""
from PIL import Image, ImageDraw

S = 512          # сторона
SS = 4           # рисуем в четыре раза крупнее и ужимаем: так края
                 # выходят гладкими без сглаживания вручную

INK = (21, 26, 23)
CREATOR_BG = (47, 63, 200)      # --accent кабинета: синий
CREATOR_MARK = (255, 255, 255)
CREATOR_DOT = (240, 130, 112)   # коралловый акцент «PrMarket»
CLIENT_BG = (240, 130, 112)     # тот же коралл, но фоном
CLIENT_MARK = (255, 255, 255)
CLIENT_DOT = (47, 63, 200)


def base(bg):
    img = Image.new("RGB", (S * SS, S * SS), bg)
    return img, ImageDraw.Draw(img)


def creator():
    """Кадр и точка записи: бот про съёмку."""
    img, d = base(CREATOR_BG)
    c = S * SS // 2
    # Рамка кадра — скруглённый квадрат в центре.
    side = int(S * SS * 0.52)
    x0, y0 = c - side // 2, c - side // 2
    w = int(S * SS * 0.035)
    d.rounded_rectangle([x0, y0, x0 + side, y0 + side],
                        radius=int(side * 0.18), outline=CREATOR_MARK, width=w)
    # Уголки кадра стёрты по горизонтали — получается «видоискатель».
    gap = int(side * 0.28)
    d.rectangle([x0 - w, c - gap // 2, x0 + w, c + gap // 2], fill=CREATOR_BG)
    d.rectangle([x0 + side - w, c - gap // 2, x0 + side + w, c + gap // 2], fill=CREATOR_BG)
    # Точка записи.
    r = int(side * 0.17)
    d.ellipse([c - r, c - r, c + r, c + r], fill=CREATOR_DOT)
    return img


def client():
    """Растущая линия: бот про просмотры и результат месяца."""
    img, d = base(CLIENT_BG)
    w = int(S * SS * 0.045)
    # Линия из четырёх точек — ломаная вверх.
    pts = [(0.24, 0.66), (0.42, 0.54), (0.58, 0.60), (0.78, 0.34)]
    xy = [(int(x * S * SS), int(y * S * SS)) for x, y in pts]
    d.line(xy, fill=CLIENT_MARK, width=w, joint="curve")
    # Круглые концы: line joint='curve' скругляет только стыки.
    for p in xy:
        d.ellipse([p[0] - w // 2, p[1] - w // 2, p[0] + w // 2, p[1] + w // 2], fill=CLIENT_MARK)
    # Последняя точка выделена — «вот сюда пришли».
    last = xy[-1]
    r = int(S * SS * 0.075)
    d.ellipse([last[0] - r, last[1] - r, last[0] + r, last[1] + r], fill=CLIENT_MARK)
    r2 = int(r * 0.45)
    d.ellipse([last[0] - r2, last[1] - r2, last[0] + r2, last[1] + r2], fill=CLIENT_DOT)
    # Основание: короткая черта снизу — «месяц», от которого растём.
    y = int(0.80 * S * SS)
    d.rounded_rectangle([int(0.22 * S * SS), y, int(0.78 * S * SS), y + w],
                        radius=w // 2, fill=(255, 255, 255, 255))
    return img


for name, img in (("creator", creator()), ("client", client())):
    out = img.resize((S, S), Image.LANCZOS)
    out.save(f"assets/avatar-{name}.png", optimize=True)
    print(name, out.size)
