from PIL import Image, ImageDraw, ImageFont
import os

S = 256
img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
d = ImageDraw.Draw(img)

# Steam dark background, rounded square
d.rounded_rectangle([6, 6, 250, 250], radius=44, fill=(23, 26, 33, 255))

# White "S"
font = None
for p in [
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
    "/System/Library/Fonts/Helvetica.ttc",
    "/System/Library/Fonts/Supplemental/Times New Roman Bold.ttf",
]:
    if os.path.exists(p):
        try:
            font = ImageFont.truetype(p, 120)
            break
        except Exception:
            pass
if font is None:
    font = ImageFont.load_default()
d.text((112, 112), "S", font=font, anchor="mm", fill=(240, 244, 250, 255))

# Green update badge bottom-right: ring plus arrowhead
d.ellipse([150, 150, 244, 244], outline=(44, 198, 90, 255), width=14)
d.polygon([(210, 152), (242, 184), (200, 196)], fill=(44, 198, 90, 255))

img.save("app.ico", sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
print("app.ico written:", os.path.getsize("app.ico"), "bytes")